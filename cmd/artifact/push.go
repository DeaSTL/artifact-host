package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/deastl/artifact-host/internal/client"
	"github.com/deastl/artifact-host/internal/model"
)

func newPushCmd(app *appContext) *cobra.Command {
	var (
		assets      []string
		assetDir    string
		entry       string
		title       string
		description string
		icon        string
		note        string
		tags        []string
		openAfter   bool
	)

	cmd := &cobra.Command{
		Use:   "push <slug> <entry-html-or-dir>",
		Short: "Upload a new version of an artifact",
		Long:  "Push a single HTML file or an entire directory (with optional extra --asset files or --dir static directory) as a new immutable version.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			slug := args[0]
			source := args[1]

			if err := model.ValidateSlug(slug); err != nil {
				return err
			}
			cleanEntry, err := model.SanitizeAssetPath(entry)
			if err != nil {
				return fmt.Errorf("invalid --entry path %q: %w", entry, err)
			}

			filesMap := make(map[string]string)

			if assetDir != "" {
				if err := collectDirectoryFiles(assetDir, filesMap); err != nil {
					return fmt.Errorf("collect --dir %s: %w", assetDir, err)
				}
			}

			info, err := os.Stat(source)
			if err != nil {
				return fmt.Errorf("stat %s: %w", source, err)
			}
			if info.IsDir() {
				if err := collectDirectoryFiles(source, filesMap); err != nil {
					return fmt.Errorf("collect directory %s: %w", source, err)
				}
			} else if info.Mode().IsRegular() {
				filesMap[cleanEntry] = source
			} else {
				return fmt.Errorf("%s is neither a regular file nor a directory", source)
			}

			for _, spec := range assets {
				dst, src, err := parseAssetSpec(spec)
				if err != nil {
					return err
				}
				filesMap[dst] = src
			}

			if _, ok := filesMap[cleanEntry]; !ok {
				return fmt.Errorf("entry file %q not found in bundle (specify --entry to override)", cleanEntry)
			}

			paths := make([]string, 0, len(filesMap))
			for p := range filesMap {
				paths = append(paths, p)
			}
			sort.Strings(paths)

			pushFiles := make([]client.PushFile, 0, len(paths))
			for _, p := range paths {
				pushFiles = append(pushFiles, client.PushFile{
					Path:       p,
					SourcePath: filesMap[p],
				})
			}

			c := app.client()
			resp, err := c.Push(cmd.Context(), slug, client.PushRequest{
				Title:       title,
				Description: description,
				Icon:        icon,
				Note:        note,
				Entry:       cleanEntry,
				Tags:        tags,
				Files:       pushFiles,
			})
			if err != nil {
				return err
			}

			if app.jsonOut {
				if err := writeJSON(app.out, resp); err != nil {
					return err
				}
			} else {
				tw := newTabWriter(app.out)
				fmt.Fprintf(tw, "Slug:\t%s\n", resp.Artifact.Slug)
				fmt.Fprintf(tw, "Version:\tv%d\n", resp.Version.Number)
				fmt.Fprintf(tw, "Files:\t%d (%s)\n", resp.Version.FileCount, formatBytes(resp.Version.TotalSize))
				if resp.URL != "" {
					fmt.Fprintf(tw, "URL:\t%s\n", resp.URL)
				}
				if resp.RawURL != "" {
					fmt.Fprintf(tw, "Raw URL:\t%s\n", resp.RawURL)
				}
				if resp.Version.PreviewStatus != "" {
					fmt.Fprintf(tw, "Preview:\t%s\n", resp.Version.PreviewStatus)
				}
				_ = tw.Flush()
			}

			if openAfter {
				targetURL := resp.URL
				if targetURL == "" {
					targetURL = c.ArtifactURL(slug, resp.Version.Number)
				}
				if err := app.openURL(targetURL); err != nil {
					return fmt.Errorf("open browser: %w", err)
				}
			}

			return nil
		},
	}

	cmd.Flags().StringArrayVar(&assets, "asset", nil, "Additional asset file to bundle as dst=src or src (repeatable)")
	cmd.Flags().StringVar(&assetDir, "dir", "", "Directory of static assets to bundle recursively")
	cmd.Flags().StringVar(&entry, "entry", "index.html", "Entry HTML path inside the artifact")
	cmd.Flags().StringVar(&title, "title", "", "Display title for the artifact")
	cmd.Flags().StringVar(&description, "description", "", "Short description of the artifact")
	cmd.Flags().StringVar(&icon, "icon", "", "Lucide icon name for the artifact")
	cmd.Flags().StringVar(&note, "note", "", "Version note / changelog entry")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "Tag to associate with the artifact (repeatable)")
	cmd.Flags().BoolVar(&openAfter, "open", false, "Open the artifact URL in the browser after pushing")

	return cmd
}

func collectDirectoryFiles(rootDir string, out map[string]string) error {
	info, err := os.Stat(rootDir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", rootDir)
	}
	return filepath.WalkDir(rootDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			// Follow symlinks to regular files if readable.
			fi, statErr := os.Stat(path)
			if statErr != nil || !fi.Mode().IsRegular() {
				return nil
			}
		}
		rel, err := filepath.Rel(rootDir, path)
		if err != nil {
			return err
		}
		cleanRel, err := model.SanitizeAssetPath(filepath.ToSlash(rel))
		if err != nil {
			return fmt.Errorf("invalid asset path %q: %w", rel, err)
		}
		out[cleanRel] = path
		return nil
	})
}

func parseAssetSpec(spec string) (string, string, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", "", errors.New("--asset value must not be empty")
	}
	var dst, src string
	if left, right, ok := strings.Cut(spec, "="); ok {
		dst = strings.TrimSpace(left)
		src = strings.TrimSpace(right)
	} else {
		src = spec
		dst = filepath.Base(spec)
	}
	if src == "" {
		return "", "", fmt.Errorf("invalid --asset %q: source path is empty", spec)
	}
	fi, err := os.Stat(src)
	if err != nil {
		return "", "", fmt.Errorf("invalid --asset %q: %w", spec, err)
	}
	if fi.IsDir() {
		return "", "", fmt.Errorf("invalid --asset %q: %s is a directory (use --dir instead)", spec, src)
	}
	cleanDst, err := model.SanitizeAssetPath(filepath.ToSlash(dst))
	if err != nil {
		return "", "", fmt.Errorf("invalid --asset destination %q: %w", dst, err)
	}
	return cleanDst, src, nil
}
