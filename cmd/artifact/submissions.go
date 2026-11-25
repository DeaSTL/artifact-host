package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/deastl/artifact-host/internal/client"
	"github.com/deastl/artifact-host/internal/model"
)

func newSubmissionsCmd(app *appContext) *cobra.Command {
	var (
		version int
		latest  bool
		id      int64
		wait    bool
		timeout time.Duration
		outPath string
	)

	cmd := &cobra.Command{
		Use:     "submissions <slug>",
		Aliases: []string{"sub", "subs"},
		Short:   "List, fetch, or wait for interactive artifact submissions",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			slug := args[0]
			if err := model.ValidateSlug(slug); err != nil {
				return err
			}
			c := app.client()
			ctx := cmd.Context()

			if id > 0 {
				return outputSubmission(ctx, app, c, id, outPath)
			}

			if wait {
				existing, err := c.Submissions(ctx, slug, client.SubmissionsOptions{
					Version: version,
				})
				if err != nil {
					return err
				}
				var afterID int64
				for _, s := range existing {
					if s.ID > afterID {
						afterID = s.ID
					}
				}
				arrived, err := c.WaitSubmission(ctx, slug, version, afterID, timeout)
				if err != nil {
					if errors.Is(err, context.DeadlineExceeded) {
						return fmt.Errorf("timed out after %s waiting for submission on %q", timeout, slug)
					}
					return err
				}
				chosen := newestSubmission(arrived)
				return outputSubmission(ctx, app, c, chosen.ID, outPath)
			}

			subs, err := c.Submissions(ctx, slug, client.SubmissionsOptions{
				Version: version,
			})
			if err != nil {
				return err
			}

			if latest {
				if len(subs) == 0 {
					return fmt.Errorf("no submissions found for %q", slug)
				}
				chosen := newestSubmission(subs)
				return outputSubmission(ctx, app, c, chosen.ID, outPath)
			}

			if app.jsonOut {
				return writeJSON(app.out, map[string]any{"submissions": subs})
			}
			if len(subs) == 0 {
				fmt.Fprintln(app.out, "No submissions found.")
				return nil
			}
			tw := newTabWriter(app.out)
			fmt.Fprintln(tw, "ID\tVERSION\tFILENAME\tCONTENT-TYPE\tSIZE\tCREATED")
			for _, s := range subs {
				fn := s.Filename
				if fn == "" {
					fn = "-"
				}
				ct := s.ContentType
				if ct == "" {
					ct = "-"
				}
				fmt.Fprintf(tw, "%d\tv%d\t%s\t%s\t%s\t%s\n",
					s.ID,
					s.Version,
					fn,
					ct,
					formatBytes(s.Size),
					formatTime(s.CreatedAt),
				)
			}
			return tw.Flush()
		},
	}

	cmd.Flags().IntVar(&version, "version", 0, "Filter submissions by artifact version")
	cmd.Flags().BoolVar(&latest, "latest", false, "Fetch the latest submission payload")
	cmd.Flags().Int64Var(&id, "id", 0, "Fetch a specific submission by ID")
	cmd.Flags().BoolVar(&wait, "wait", false, "Wait for a new submission to arrive")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "Maximum time to wait when --wait is enabled")
	cmd.Flags().StringVar(&outPath, "out", "", "Write submission payload to a file path (use '-' for stdout)")

	return cmd
}

func newestSubmission(subs []model.SubmissionMeta) model.SubmissionMeta {
	best := subs[0]
	for _, s := range subs[1:] {
		if s.ID > best.ID {
			best = s
		}
	}
	return best
}

func outputSubmission(ctx context.Context, app *appContext, c *client.Client, id int64, outPath string) error {
	if outPath != "" && outPath != "-" {
		dl, err := c.GetSubmission(ctx, id)
		if err != nil {
			return err
		}
		if dir := filepath.Dir(outPath); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("create output directory %s: %w", dir, err)
			}
		}
		if err := os.WriteFile(outPath, dl.Data, 0o644); err != nil {
			return fmt.Errorf("write submission to %s: %w", outPath, err)
		}
		if app.jsonOut {
			return writeJSON(app.out, map[string]any{
				"id":           dl.ID,
				"filename":     dl.Filename,
				"content_type": dl.ContentType,
				"size":         len(dl.Data),
				"out":          outPath,
			})
		}
		fmt.Fprintf(app.out, "Saved submission %d (%s) to %s\n", dl.ID, formatBytes(int64(len(dl.Data))), outPath)
		return nil
	}

	if app.jsonOut {
		sub, err := c.GetSubmissionMeta(ctx, id)
		if err != nil {
			return err
		}
		return writeJSON(app.out, sub)
	}

	dl, err := c.GetSubmission(ctx, id)
	if err != nil {
		return err
	}
	_, err = app.out.Write(dl.Data)
	return err
}
