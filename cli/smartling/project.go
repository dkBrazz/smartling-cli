package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/tabwriter"
	"text/template"

	"github.com/99designs/smartling"
	"github.com/codegangsta/cli"
)

var ProjectCommand = cli.Command{
	Name:  "project",
	Usage: "manage local project files",
	Before: func(c *cli.Context) (err error) {
		return loadProjectErr
	},
	After: func(c *cli.Context) error {
		cleanupTempFiles()
		return nil
	},
	Subcommands: []cli.Command{
		projectStatusCommand,
		projectPullCommand,
		projectPushCommand,
	},
}

var projectStatusCommand = cli.Command{
	Name:        "status",
	Usage:       "show the status of the project's local files",
	Description: "status",
	Action: func(c *cli.Context) {

		projectFilepaths := ProjectConfig.Files
		locales, err := client.Locales()
		panicIfErr(err)

		var wg sync.WaitGroup
		statuses := make(map[string]map[string]smartling.FileStatus)

		for _, projectFilepath := range projectFilepaths {
			tmpfile, err := uploadAsTempFile(
				localRelativeFilePath(projectFilepath),
				filetypeForProjectFile(projectFilepath),
				ProjectConfig.FileConfig.ParserConfig,
			)
			panicIfErr(err)

			for _, l := range locales {
				wg.Add(1)
				go func(tmpfile, locale, projectFilepath string) {
					defer wg.Done()

					fs, err := client.Status(tmpfile, locale)
					panicIfErr(err)

					_, ok := statuses[projectFilepath]
					if !ok {
						mm := make(map[string]smartling.FileStatus)
						statuses[projectFilepath] = mm
					}
					statuses[projectFilepath][locale] = fs
				}(tmpfile, l.Locale, projectFilepath)
			}
		}
		wg.Wait()

		fmt.Print("\n")
		fmt.Println("Translation counts: Awaiting Authorization -> In Progress -> Completed")
		fmt.Print("\n")

		// Format in columns
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', 0)

		fmt.Fprint(w, " ")
		for _, locale := range locales {
			fmt.Fprint(w, "\t", locale.Locale)
		}
		fmt.Fprint(w, "\n")

		for _, projectFilepath := range projectFilepaths {
			fmt.Fprint(w, projectFilepath)
			for _, locale := range locales {
				status := statuses[projectFilepath][locale.Locale]
				fmt.Fprint(w, "\t", status.AwaitingAuthorizationCount(), "->", status.InProgressCount(), "->", status.CompletedStringCount)
			}
			fmt.Fprint(w, "\n")
		}
		w.Flush()
	},
}

var projectPullCommand = cli.Command{
	Name:  "pull",
	Usage: "translate local project files using Smartling as a translation memory",

	Action: func(c *cli.Context) {
		locales, err := client.Locales()
		panicIfErr(err)

		var wg sync.WaitGroup
		for _, projectFilepath := range ProjectConfig.Files {
			for _, l := range locales {
				wg.Add(1)
				go func(locale, projectFilepath string) {
					defer wg.Done()

					hit, b, err, _ := translateViaCache(
						locale,
						localRelativeFilePath(projectFilepath),
						filetypeForProjectFile(projectFilepath),
						ProjectConfig.FileConfig.ParserConfig,
					)
					panicIfErr(err)

					fp := localPullFilePath(projectFilepath, locale)
					cached := ""
					if hit {
						cached = "(using cache)"
					}
					fmt.Println(fp, cached)
					err = ioutil.WriteFile(fp, b, 0644)
					panicIfErr(err)
				}(l.Locale, projectFilepath)
			}
		}
		wg.Wait()
	},
}

func cleanPrefix(s string) string {
	s = filepath.Clean("/" + s)
	if s == "/" {
		return ""
	}
	return s
}

func getPrefix(c *cli.Context) string {
	prefix := c.String("prefix")
	if prefix == "" {
		prefix = pushPrefix()
	}
	prefix = cleanPrefix(prefix)

	if prefix != "" {
		fmt.Println("Using prefix", prefix)
	}
	return prefix
}

var projectPushCommand = cli.Command{
	Name:        "push",
	Usage:       "upload local project files with new strings, using the git branch or user name as a prefix",
	Description: "push [prefix]",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "prefix",
			Usage: "Use the specified prefix instead of the default",
		},
	},
	Action: func(c *cli.Context) {
		prefix := getPrefix(c)

		locales, err := client.Locales()
		panicIfErr(err)
		firstLocale := locales[0].Locale

		var wg sync.WaitGroup
		for _, projectFilepath := range ProjectConfig.Files {
			wg.Add(1)
			go func(projectFilepath string) {
				defer wg.Done()

				remoteFile := filepath.Clean(prefix + "/" + projectFilepath)

				_, err := client.Upload(projectFilepath, &smartling.UploadRequest{
					FileUri:      remoteFile,
					FileType:     filetypeForProjectFile(projectFilepath),
					ParserConfig: ProjectConfig.FileConfig.ParserConfig,
				})
				panicIfErr(err)

				fs, err := client.Status(remoteFile, firstLocale)
				panicIfErr(err)

				hasNewStrings := true
				// when using a prefix, we only want to upload files with new strings
				if prefix != "" {
					if fs.AwaitingAuthorizationCount() == 0 {
						err := client.Delete(remoteFile)
						panicIfErr(err)
						hasNewStrings = false
					}
				}
				if hasNewStrings {
					fmt.Printf("%3d unauthorised strings in %s\n", fs.AwaitingAuthorizationCount(), remoteFile)
				}

			}(projectFilepath)
		}
		wg.Wait()
	},
}

func filetypeForProjectFile(projectFilepath string) smartling.FileType {
	ft := smartling.FileTypeByExtension(filepath.Ext(projectFilepath))
	if ft == "" {
		ft = ProjectConfig.FileConfig.FileType
	}
	if ft == "" {
		log.Panicln("Can't determine file type for " + projectFilepath)
	}

	return ft
}

type FilenameParts struct {
	Path           string
	Base           string
	Dir            string
	Ext            string
	PathWithoutExt string
	Locale         string
}

func localRelativeFilePath(remotepath string) string {
	fp, err := filepath.Rel(".", filepath.Join(ProjectConfig.path, remotepath))
	panicIfErr(err)
	return fp
}

func localPullFilePath(p, locale string) string {
	parts := FilenameParts{
		Path:   p,
		Dir:    filepath.Dir(p),
		Base:   filepath.Base(p),
		Ext:    filepath.Ext(p),
		Locale: locale,
	}

	dt := defaultPullDestination
	if dt != "" {
		dt = ProjectConfig.FileConfig.PullFilePath
	}

	out := bytes.NewBufferString("")
	tmpl := template.New("name")
	tmpl.Funcs(template.FuncMap{
		"TrimSuffix": strings.TrimSuffix,
	})
	_, err := tmpl.Parse(dt)
	panicIfErr(err)

	err = tmpl.Execute(out, parts)
	panicIfErr(err)

	return localRelativeFilePath(out.String())
}