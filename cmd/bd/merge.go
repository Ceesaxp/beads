package main

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/types"
)

var mergeCmd = &cobra.Command{
	Use:   "merge [source-id...] --into [target-id]",
	Short: "Merge duplicate issues into a single issue",
	Long: `Merge one or more source issues into a target issue.

This command:
1. Validates all issues exist and no self-merge
2. Closes source issues with reason 'Merged into bd-X'
3. Migrates all dependencies from sources to target
4. Updates text references in all issue descriptions/notes

Example:
  bd merge bd-42 bd-43 --into bd-42
  bd merge bd-10 bd-11 bd-12 --into bd-10 --dry-run`,
	Args: cobra.MinimumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		// Check daemon mode first before accessing store
		if daemonClient != nil {
			fmt.Fprintf(os.Stderr, "Error: merge command not yet supported in daemon mode (see bd-190)\n")
			os.Exit(1)
		}

		targetID, _ := cmd.Flags().GetString("into")
		if targetID == "" {
			fmt.Fprintf(os.Stderr, "Error: --into flag is required\n")
			os.Exit(1)
		}

		sourceIDs := args
		dryRun, _ := cmd.Flags().GetBool("dry-run")

		// Validate merge operation
		if err := validateMerge(targetID, sourceIDs); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

		// Direct mode
		ctx := context.Background()

		if dryRun {
			if !jsonOutput {
				fmt.Println("Dry run - validation passed, no changes made")
				fmt.Printf("Would merge: %s into %s\n", strings.Join(sourceIDs, ", "), targetID)
			}
			return
		}

		// Perform merge
		if err := performMerge(ctx, targetID, sourceIDs); err != nil {
			fmt.Fprintf(os.Stderr, "Error performing merge: %v\n", err)
			os.Exit(1)
		}

		// Schedule auto-flush
		markDirtyAndScheduleFlush()

		if jsonOutput {
			result := map[string]interface{}{
				"target_id":  targetID,
				"source_ids": sourceIDs,
				"merged":     len(sourceIDs),
			}
			outputJSON(result)
		} else {
			green := color.New(color.FgGreen).SprintFunc()
			fmt.Printf("%s Merged %d issue(s) into %s\n", green("✓"), len(sourceIDs), targetID)
		}
	},
}

func init() {
	mergeCmd.Flags().String("into", "", "Target issue ID to merge into (required)")
	mergeCmd.Flags().Bool("dry-run", false, "Validate without making changes")
	rootCmd.AddCommand(mergeCmd)
}

// validateMerge checks that merge operation is valid
func validateMerge(targetID string, sourceIDs []string) error {
	ctx := context.Background()

	// Check target exists
	target, err := store.GetIssue(ctx, targetID)
	if err != nil {
		return fmt.Errorf("target issue not found: %s", targetID)
	}
	if target == nil {
		return fmt.Errorf("target issue not found: %s", targetID)
	}

	// Check all sources exist and validate no self-merge
	for _, sourceID := range sourceIDs {
		if sourceID == targetID {
			return fmt.Errorf("cannot merge issue into itself: %s", sourceID)
		}

		source, err := store.GetIssue(ctx, sourceID)
		if err != nil {
			return fmt.Errorf("source issue not found: %s", sourceID)
		}
		if source == nil {
			return fmt.Errorf("source issue not found: %s", sourceID)
		}
	}

	return nil
}

// performMerge executes the merge operation
// TODO(bd-202): Add transaction support for atomicity
func performMerge(ctx context.Context, targetID string, sourceIDs []string) error {
	// Step 1: Migrate dependencies from source issues to target
	for _, sourceID := range sourceIDs {
		// Get all dependencies where source is the dependent (source depends on X)
		deps, err := store.GetDependencyRecords(ctx, sourceID)
		if err != nil {
			return fmt.Errorf("failed to get dependencies for %s: %w", sourceID, err)
		}

		// Migrate each dependency to target
		for _, dep := range deps {
			// Skip if target already has this dependency
			existingDeps, err := store.GetDependencyRecords(ctx, targetID)
			if err != nil {
				return fmt.Errorf("failed to check target dependencies: %w", err)
			}

			alreadyExists := false
			for _, existing := range existingDeps {
				if existing.DependsOnID == dep.DependsOnID && existing.Type == dep.Type {
					alreadyExists = true
					break
				}
			}

			if !alreadyExists && dep.DependsOnID != targetID {
				// Add dependency to target
				newDep := &types.Dependency{
					IssueID:     targetID,
					DependsOnID: dep.DependsOnID,
					Type:        dep.Type,
					CreatedAt:   time.Now(),
					CreatedBy:   actor,
				}
				if err := store.AddDependency(ctx, newDep, actor); err != nil {
					return fmt.Errorf("failed to migrate dependency %s -> %s: %w", targetID, dep.DependsOnID, err)
				}
			}
		}

		// Get all dependencies where source is the dependency (X depends on source)
		allDeps, err := store.GetAllDependencyRecords(ctx)
		if err != nil {
			return fmt.Errorf("failed to get all dependencies: %w", err)
		}

		for issueID, depList := range allDeps {
			for _, dep := range depList {
				if dep.DependsOnID == sourceID {
					// Remove old dependency
					if err := store.RemoveDependency(ctx, issueID, sourceID, actor); err != nil {
						// Ignore "not found" errors as they may have been cleaned up
						if !strings.Contains(err.Error(), "not found") {
							return fmt.Errorf("failed to remove dependency %s -> %s: %w", issueID, sourceID, err)
						}
					}

					// Add new dependency to target (if not self-reference)
					if issueID != targetID {
						newDep := &types.Dependency{
							IssueID:     issueID,
							DependsOnID: targetID,
							Type:        dep.Type,
							CreatedAt:   time.Now(),
							CreatedBy:   actor,
						}
						if err := store.AddDependency(ctx, newDep, actor); err != nil {
							// Ignore if dependency already exists
							if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
								return fmt.Errorf("failed to add dependency %s -> %s: %w", issueID, targetID, err)
							}
						}
					}
				}
			}
		}
	}

	// Step 2: Update text references in all issues
	if err := updateMergeTextReferences(ctx, sourceIDs, targetID); err != nil {
		return fmt.Errorf("failed to update text references: %w", err)
	}

	// Step 3: Close source issues
	for _, sourceID := range sourceIDs {
		reason := fmt.Sprintf("Merged into %s", targetID)
		if err := store.CloseIssue(ctx, sourceID, reason, actor); err != nil {
			return fmt.Errorf("failed to close source issue %s: %w", sourceID, err)
		}
	}

	return nil
}

// updateMergeTextReferences updates text references from source IDs to target ID
func updateMergeTextReferences(ctx context.Context, sourceIDs []string, targetID string) error {
	// Get all issues to scan for references
	allIssues, err := store.SearchIssues(ctx, "", types.IssueFilter{})
	if err != nil {
		return fmt.Errorf("failed to get all issues: %w", err)
	}

	updatedCount := 0
	for _, issue := range allIssues {
		// Skip source issues (they're being closed anyway)
		isSource := false
		for _, srcID := range sourceIDs {
			if issue.ID == srcID {
				isSource = true
				break
			}
		}
		if isSource {
			continue
		}

		updates := make(map[string]interface{})

		// Check each source ID for references
		for _, sourceID := range sourceIDs {
			// Build regex pattern to match issue IDs with word boundaries
			idPattern := `(^|[^A-Za-z0-9_-])(` + regexp.QuoteMeta(sourceID) + `)($|[^A-Za-z0-9_-])`
			re := regexp.MustCompile(idPattern)
			replacementText := `$1` + targetID + `$3`

			// Update description
			if issue.Description != "" && re.MatchString(issue.Description) {
				if _, exists := updates["description"]; !exists {
					updates["description"] = issue.Description
				}
				updates["description"] = re.ReplaceAllString(updates["description"].(string), replacementText)
			}

			// Update notes
			if issue.Notes != "" && re.MatchString(issue.Notes) {
				if _, exists := updates["notes"]; !exists {
					updates["notes"] = issue.Notes
				}
				updates["notes"] = re.ReplaceAllString(updates["notes"].(string), replacementText)
			}

			// Update design
			if issue.Design != "" && re.MatchString(issue.Design) {
				if _, exists := updates["design"]; !exists {
					updates["design"] = issue.Design
				}
				updates["design"] = re.ReplaceAllString(updates["design"].(string), replacementText)
			}

			// Update acceptance criteria
			if issue.AcceptanceCriteria != "" && re.MatchString(issue.AcceptanceCriteria) {
				if _, exists := updates["acceptance_criteria"]; !exists {
					updates["acceptance_criteria"] = issue.AcceptanceCriteria
				}
				updates["acceptance_criteria"] = re.ReplaceAllString(updates["acceptance_criteria"].(string), replacementText)
			}
		}

		// Apply updates if any
		if len(updates) > 0 {
			if err := store.UpdateIssue(ctx, issue.ID, updates, actor); err != nil {
				return fmt.Errorf("failed to update issue %s: %w", issue.ID, err)
			}
			updatedCount++
		}
	}

	return nil
}
