package main

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/deastl/artifact-host/internal/model"
)

func newListCmd(app *appContext) *cobra.Command {
	var (
		search string
		tag    string
		limit  int
		offset int
	)

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List hosted artifacts",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			resp, err := app.client().List(cmd.Context(), model.ListFilter{
				Search: search,
				Tag:    tag,
				Limit:  limit,
				Offset: offset,
			})
			if err != nil {
				return err
			}
			if app.jsonOut {
				return writeJSON(app.out, resp)
			}
			if len(resp.Artifacts) == 0 {
				fmt.Fprintln(app.out, "No artifacts found.")
				return nil
			}
			tw := newTabWriter(app.out)
			fmt.Fprintln(tw, "SLUG\tTITLE\tVERSION\tSUBMISSIONS\tPREVIEW\tTAGS\tUPDATED")
			for _, a := range resp.Artifacts {
				title := a.Title
				if title == "" {
					title = "-"
				}
				preview := a.LatestPreviewStatus
				if preview == "" {
					preview = "-"
				}
				tagsStr := "-"
				if len(a.Tags) > 0 {
					tagsStr = strings.Join(a.Tags, ",")
				}
				fmt.Fprintf(tw, "%s\t%s\tv%d\t%d\t%s\t%s\t%s\n",
					a.Slug,
					title,
					a.EffectiveVersion(),
					a.SubmissionCount,
					preview,
					tagsStr,
					formatTime(a.UpdatedAt),
				)
			}
			return tw.Flush()
		},
	}

	cmd.Flags().StringVar(&search, "search", "", "Filter artifacts by search query")
	cmd.Flags().StringVar(&tag, "tag", "", "Filter artifacts by tag")
	cmd.Flags().IntVar(&limit, "limit", 0, "Maximum number of artifacts to return")
	cmd.Flags().IntVar(&offset, "offset", 0, "Number of artifacts to skip")

	return cmd
}

func newInfoCmd(app *appContext) *cobra.Command {
	var version int

	cmd := &cobra.Command{
		Use:   "info <slug>",
		Short: "Show detailed metadata for an artifact",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			detail, err := app.client().Info(cmd.Context(), args[0], version)
			if err != nil {
				return err
			}
			if app.jsonOut {
				return writeJSON(app.out, detail)
			}

			a := detail.Artifact
			tw := newTabWriter(app.out)
			fmt.Fprintf(tw, "Slug:\t%s\n", a.Slug)
			if a.Title != "" {
				fmt.Fprintf(tw, "Title:\t%s\n", a.Title)
			}
			if a.Description != "" {
				fmt.Fprintf(tw, "Description:\t%s\n", a.Description)
			}
			if a.Icon != "" {
				fmt.Fprintf(tw, "Icon:\t%s\n", a.Icon)
			}
			if len(a.Tags) > 0 {
				fmt.Fprintf(tw, "Tags:\t%s\n", strings.Join(a.Tags, ", "))
			}
			fmt.Fprintf(tw, "Latest Version:\tv%d\n", a.LatestVersion)
			if a.ActiveVersion > 0 {
				fmt.Fprintf(tw, "Active Version:\tv%d\n", a.ActiveVersion)
			}
			fmt.Fprintf(tw, "Versions:\t%d\n", a.VersionCount)
			fmt.Fprintf(tw, "Submissions:\t%d\n", a.SubmissionCount)
			fmt.Fprintf(tw, "Updated:\t%s\n", formatTime(a.UpdatedAt))
			_ = tw.Flush()

			cur := detail.CurrentVersion
			if cur == nil && len(detail.Versions) > 0 {
				cur = &detail.Versions[0]
			}
			if cur != nil {
				fmt.Fprintln(app.out)
				vtw := newTabWriter(app.out)
				fmt.Fprintf(vtw, "Selected Version:\tv%d\n", cur.Number)
				fmt.Fprintf(vtw, "Entry File:\t%s\n", cur.EntryFile)
				if cur.Note != "" {
					fmt.Fprintf(vtw, "Note:\t%s\n", cur.Note)
				}
				fmt.Fprintf(vtw, "Size:\t%s (%d files)\n", formatBytes(cur.TotalSize), cur.FileCount)
				if cur.ContentHash != "" {
					fmt.Fprintf(vtw, "Content Hash:\t%s\n", cur.ContentHash)
				}
				fmt.Fprintf(vtw, "Preview Status:\t%s\n", cur.PreviewStatus)
				if cur.PreviewError != "" {
					fmt.Fprintf(vtw, "Preview Error:\t%s\n", cur.PreviewError)
				}
				fmt.Fprintf(vtw, "Created:\t%s\n", formatTime(cur.CreatedAt))
				_ = vtw.Flush()

				if len(cur.Assets) > 0 {
					fmt.Fprintln(app.out)
					atw := newTabWriter(app.out)
					fmt.Fprintln(atw, "ASSET PATH\tSIZE\tCONTENT-TYPE")
					for _, asset := range cur.Assets {
						fmt.Fprintf(atw, "%s\t%s\t%s\n", asset.Path, formatBytes(asset.Size), asset.ContentType)
					}
					_ = atw.Flush()
				}
			}

			return nil
		},
	}

	cmd.Flags().IntVar(&version, "version", 0, "Inspect a specific version number")
	return cmd
}

func newVersionsCmd(app *appContext) *cobra.Command {
	return &cobra.Command{
		Use:   "versions <slug>",
		Short: "List all versions of an artifact",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			versions, err := app.client().Versions(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if app.jsonOut {
				return writeJSON(app.out, map[string]any{"versions": versions})
			}
			if len(versions) == 0 {
				fmt.Fprintln(app.out, "No versions found.")
				return nil
			}
			tw := newTabWriter(app.out)
			fmt.Fprintln(tw, "VERSION\tFILES\tSIZE\tPREVIEW\tNOTE\tCREATED")
			for _, v := range versions {
				note := v.Note
				if note == "" {
					note = "-"
				}
				preview := v.PreviewStatus
				if preview == "" {
					preview = "-"
				}
				fmt.Fprintf(tw, "v%d\t%d\t%s\t%s\t%s\t%s\n",
					v.Number,
					v.FileCount,
					formatBytes(v.TotalSize),
					preview,
					note,
					formatTime(v.CreatedAt),
				)
			}
			return tw.Flush()
		},
	}
}

func newRollbackCmd(app *appContext) *cobra.Command {
	var note string

	cmd := &cobra.Command{
		Use:   "rollback <slug> <version>",
		Short: "Roll back an artifact to a previous version",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			slug := args[0]
			ver, err := parseVersionArg(args[1])
			if err != nil {
				return err
			}
			resp, err := app.client().Rollback(cmd.Context(), slug, ver, note)
			if err != nil {
				return err
			}
			if app.jsonOut {
				return writeJSON(app.out, resp)
			}
			tw := newTabWriter(app.out)
			fmt.Fprintf(tw, "Slug:\t%s\n", resp.Artifact.Slug)
			fmt.Fprintf(tw, "Rolled Back To:\tv%d\n", ver)
			fmt.Fprintf(tw, "New Version:\tv%d\n", resp.Version.Number)
			if resp.URL != "" {
				fmt.Fprintf(tw, "URL:\t%s\n", resp.URL)
			}
			return tw.Flush()
		},
	}

	cmd.Flags().StringVar(&note, "note", "", "Optional note for the rollback version")
	return cmd
}

func newDeleteCmd(app *appContext) *cobra.Command {
	var (
		version int
		yes     bool
	)

	cmd := &cobra.Command{
		Use:     "delete <slug>",
		Aliases: []string{"rm"},
		Short:   "Delete an artifact or a specific version",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			slug := args[0]
			if !yes {
				if version > 0 {
					fmt.Fprintf(app.errOut, "Delete version v%d of artifact %q? [y/N]: ", version, slug)
				} else {
					fmt.Fprintf(app.errOut, "Delete artifact %q and all its versions? [y/N]: ", slug)
				}
				reader := bufio.NewReader(app.in)
				line, _ := reader.ReadString('\n')
				ans := strings.ToLower(strings.TrimSpace(line))
				if ans != "y" && ans != "yes" {
					fmt.Fprintln(app.out, "Aborted.")
					return nil
				}
			}

			c := app.client()
			if version > 0 {
				if err := c.DeleteVersion(cmd.Context(), slug, version); err != nil {
					return err
				}
				if app.jsonOut {
					return writeJSON(app.out, map[string]any{
						"deleted": true,
						"slug":    slug,
						"version": version,
					})
				}
				fmt.Fprintf(app.out, "Deleted %s v%d.\n", slug, version)
				return nil
			}

			if err := c.Delete(cmd.Context(), slug); err != nil {
				return err
			}
			if app.jsonOut {
				return writeJSON(app.out, map[string]any{
					"deleted": true,
					"slug":    slug,
				})
			}
			fmt.Fprintf(app.out, "Deleted %s.\n", slug)
			return nil
		},
	}

	cmd.Flags().IntVar(&version, "version", 0, "Delete only this specific version number")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation prompt")
	return cmd
}

func newPullCmd(app *appContext) *cobra.Command {
	var (
		version int
		outDir  string
	)

	cmd := &cobra.Command{
		Use:   "pull <slug>",
		Short: "Download all assets of an artifact version to a local directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			slug := args[0]
			res, err := app.client().Pull(cmd.Context(), slug, version, outDir)
			if err != nil {
				return err
			}
			if app.jsonOut {
				return writeJSON(app.out, res)
			}
			tw := newTabWriter(app.out)
			fmt.Fprintf(tw, "Slug:\t%s\n", res.Slug)
			fmt.Fprintf(tw, "Version:\tv%d\n", res.Version)
			fmt.Fprintf(tw, "Output:\t%s\n", res.OutDir)
			fmt.Fprintf(tw, "Files:\t%d (%s)\n", len(res.Files), formatBytes(res.TotalBytes))
			return tw.Flush()
		},
	}

	cmd.Flags().IntVar(&version, "version", 0, "Version number to pull (defaults to effective/latest)")
	cmd.Flags().StringVar(&outDir, "out", "", "Destination directory (defaults to ./<slug>-v<N>)")
	return cmd
}

func newPreviewCmd(app *appContext) *cobra.Command {
	var (
		version int
		refresh bool
	)

	cmd := &cobra.Command{
		Use:   "preview <slug>",
		Short: "Inspect or refresh the rendered preview for an artifact version",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			slug := args[0]
			c := app.client()

			detail, err := c.Info(cmd.Context(), slug, version)
			if err != nil {
				return err
			}

			verNum := version
			var cur *model.Version
			if detail.CurrentVersion != nil && (version == 0 || detail.CurrentVersion.Number == version) {
				cur = detail.CurrentVersion
				verNum = cur.Number
			} else {
				if verNum == 0 {
					verNum = detail.Artifact.EffectiveVersion()
				}
				for i := range detail.Versions {
					if detail.Versions[i].Number == verNum {
						cur = &detail.Versions[i]
						break
					}
				}
			}
			if verNum <= 0 {
				return fmt.Errorf("artifact %q has no versions", slug)
			}

			if refresh {
				ref, err := c.RefreshPreview(cmd.Context(), slug, verNum)
				if err != nil {
					return err
				}
				if app.jsonOut {
					return writeJSON(app.out, map[string]any{
						"slug":        slug,
						"version":     verNum,
						"status":      ref.Status,
						"preview_url": ref.PreviewURL,
					})
				}
				tw := newTabWriter(app.out)
				fmt.Fprintf(tw, "Slug:\t%s\n", slug)
				fmt.Fprintf(tw, "Version:\tv%d\n", verNum)
				fmt.Fprintf(tw, "Status:\t%s\n", ref.Status)
				if ref.PreviewURL != "" {
					fmt.Fprintf(tw, "Preview URL:\t%s\n", ref.PreviewURL)
				}
				return tw.Flush()
			}

			status := detail.Artifact.LatestPreviewStatus
			previewErr := ""
			previewHash := ""
			var updatedAt string
			if cur != nil {
				status = cur.PreviewStatus
				previewErr = cur.PreviewError
				previewHash = cur.PreviewHash
				if cur.PreviewUpdatedAt != nil {
					updatedAt = formatTime(*cur.PreviewUpdatedAt)
				}
			}

			if app.jsonOut {
				out := map[string]any{
					"slug":    slug,
					"version": verNum,
					"status":  status,
				}
				if previewHash != "" {
					out["preview_hash"] = previewHash
				}
				if previewErr != "" {
					out["preview_error"] = previewErr
				}
				if cur != nil && cur.PreviewUpdatedAt != nil {
					out["preview_updated_at"] = cur.PreviewUpdatedAt
				}
				return writeJSON(app.out, out)
			}

			tw := newTabWriter(app.out)
			fmt.Fprintf(tw, "Slug:\t%s\n", slug)
			fmt.Fprintf(tw, "Version:\tv%d\n", verNum)
			fmt.Fprintf(tw, "Status:\t%s\n", status)
			if previewHash != "" {
				fmt.Fprintf(tw, "Hash:\t%s\n", previewHash)
			}
			if updatedAt != "" {
				fmt.Fprintf(tw, "Updated:\t%s\n", updatedAt)
			}
			if previewErr != "" {
				fmt.Fprintf(tw, "Error:\t%s\n", previewErr)
			}
			return tw.Flush()
		},
	}

	cmd.Flags().IntVar(&version, "version", 0, "Target version number (defaults to effective/latest)")
	cmd.Flags().BoolVar(&refresh, "refresh", false, "Trigger preview regeneration")
	return cmd
}

func newOpenCmd(app *appContext) *cobra.Command {
	var (
		version int
		raw     bool
	)

	cmd := &cobra.Command{
		Use:   "open <slug>",
		Short: "Open an artifact URL in the default browser using xdg-open",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			slug := args[0]
			if err := model.ValidateSlug(slug); err != nil {
				return err
			}
			c := app.client()

			var targetURL string
			if raw {
				detail, err := c.Info(cmd.Context(), slug, version)
				if err != nil {
					return err
				}
				verNum := version
				entryFile := "index.html"
				if detail.CurrentVersion != nil && (version == 0 || detail.CurrentVersion.Number == version) {
					verNum = detail.CurrentVersion.Number
					if detail.CurrentVersion.EntryFile != "" {
						entryFile = detail.CurrentVersion.EntryFile
					}
				} else {
					if verNum == 0 {
						verNum = detail.Artifact.EffectiveVersion()
					}
					for _, v := range detail.Versions {
						if v.Number == verNum {
							if v.EntryFile != "" {
								entryFile = v.EntryFile
							}
							break
						}
					}
				}
				if verNum <= 0 {
					return fmt.Errorf("artifact %q has no versions", slug)
				}
				targetURL = c.RawAssetURL(slug, verNum, entryFile)
			} else {
				targetURL = c.ArtifactURL(slug, version)
			}

			if err := app.openURL(targetURL); err != nil {
				return fmt.Errorf("open %s: %w", targetURL, err)
			}
			if app.jsonOut {
				return writeJSON(app.out, map[string]string{"url": targetURL})
			}
			fmt.Fprintln(app.out, targetURL)
			return nil
		},
	}

	cmd.Flags().IntVar(&version, "version", 0, "Open a specific version number")
	cmd.Flags().BoolVar(&raw, "raw", false, "Open the raw entry asset URL instead of the viewer page")
	return cmd
}

func parseVersionArg(raw string) (int, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(raw), "v")
	v, err := strconv.Atoi(trimmed)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("invalid version %q: must be a positive integer", raw)
	}
	return v, nil
}
