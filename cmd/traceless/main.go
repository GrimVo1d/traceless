// Command traceless inspects and removes metadata from files.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"

	"traceless/internal/core"
	"traceless/internal/format"
)

const usage = `Usage:
  traceless inspect [-json] FILE... list the metadata in each file
  traceless strip [flags] FILE...   write a copy of each file without metadata
  traceless verify FILE...          check that files contain no metadata

Strip flags:
  -o PATH     output path (one input file only)
  -inplace    replace the input files

By default, strip writes NAME.clean.EXT next to each input, readable only by
you, and never overwrites an existing file.

-inplace keeps the file's permissions and refuses files with other hard
links. The original data may stay on disk until the space is reused.
`

// Exit codes.
const (
	exitOK          = 0
	exitFail        = 1 // a file failed, or verify found metadata
	exitUsage       = 2
	exitInterrupted = 130
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run executes one command and returns the exit code.
func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd, args := args[0], args[1:]
	code := exitOK
	switch cmd {
	case "inspect":
		code = runInspect(ctx, args)
	case "strip":
		code = runStrip(ctx, args)
	case "verify":
		code = runVerify(ctx, args)
	case "help", "-h", "-help", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "traceless: unknown command %s\n\n%s", safe(cmd), usage)
		code = exitUsage
	}
	if ctx.Err() != nil {
		code = exitInterrupted
	}
	return code
}

func runInspect(ctx context.Context, args []string) int {
	fs := newFlagSet("inspect")
	asJSON := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() == 0 {
		return usageError(err)
	}
	if *asJSON {
		results, code := inspectAll(ctx, fs.Args())
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(results); err != nil {
			return fail("stdout", err)
		}
		return code
	}
	code := exitOK
	for _, path := range fs.Args() {
		if ctx.Err() != nil {
			break
		}
		h, fields, err := core.InspectFile(ctx, path)
		if err != nil {
			code = fail(path, err)
			continue
		}
		if len(fields) == 0 {
			fmt.Printf("%s (%s): no metadata found\n", safe(path), h.Name())
			continue
		}
		fmt.Printf("%s (%s):\n", safe(path), h.Name())
		for _, f := range fields {
			fmt.Printf("  %s: %s\n", safe(f.Name), safe(f.Value))
		}
	}
	return code
}

func runStrip(ctx context.Context, args []string) int {
	fs := newFlagSet("strip")
	out := fs.String("o", "", "output path (one input file only)")
	inplace := fs.Bool("inplace", false, "replace the input files")
	if err := fs.Parse(args); err != nil || fs.NArg() == 0 {
		return usageError(err)
	}
	if *out != "" && *inplace {
		return usageError(errors.New("-o and -inplace cannot be combined"))
	}
	if *out != "" && fs.NArg() > 1 {
		return usageError(errors.New("-o needs exactly one input file"))
	}

	code := exitOK
	for _, path := range fs.Args() {
		if ctx.Err() != nil {
			break
		}
		src, dst := path, *out
		switch {
		case *inplace:
			// Resolve a symlink once and use the result for both reading and
			// writing, so retargeting the link midway can't mix up files.
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				code = fail(path, err)
				continue
			}
			src, dst = resolved, resolved
		case dst == "":
			ext := filepath.Ext(path)
			dst = strings.TrimSuffix(path, ext) + ".clean" + ext
		}
		var h format.Handler
		var err error
		if *inplace {
			h, err = core.StripInPlace(ctx, src)
		} else {
			h, err = core.StripFile(ctx, src, dst)
		}
		if err != nil {
			code = fail(path, err)
			continue
		}
		fmt.Printf("%s -> %s (%s)\n", safe(path), safe(dst), h.Name())
	}
	return code
}

func runVerify(ctx context.Context, args []string) int {
	fs := newFlagSet("verify")
	if err := fs.Parse(args); err != nil || fs.NArg() == 0 {
		return usageError(err)
	}
	code := exitOK
	for _, path := range fs.Args() {
		if ctx.Err() != nil {
			break
		}
		h, err := core.VerifyFile(ctx, path)
		if err != nil {
			code = fail(path, err)
			continue
		}
		fmt.Printf("%s (%s): clean\n", safe(path), h.Name())
	}
	return code
}

// inspectResult is the JSON output for one file. Fields is empty, never
// null, when a file has no metadata or fails.
type inspectResult struct {
	File   string         `json:"file"`
	Format string         `json:"format,omitempty"`
	Fields []format.Field `json:"fields"`
	Error  string         `json:"error,omitempty"`
}

// inspectAll inspects each path and returns the results and the exit code.
func inspectAll(ctx context.Context, paths []string) ([]inspectResult, int) {
	results := make([]inspectResult, 0, len(paths))
	code := exitOK
	for _, path := range paths {
		if ctx.Err() != nil {
			break
		}
		r := inspectResult{File: path, Fields: []format.Field{}}
		h, fields, err := core.InspectFile(ctx, path)
		if h != nil {
			r.Format = h.Name()
		}
		if err != nil {
			r.Error = err.Error()
			code = exitFail
		} else if fields != nil {
			r.Fields = fields
		}
		results = append(results, r)
	}
	return results, code
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func usageError(err error) int {
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintf(os.Stderr, "traceless: %s\n\n", safe(err.Error()))
	}
	fmt.Fprint(os.Stderr, usage)
	return exitUsage
}

func fail(path string, err error) int {
	fmt.Fprintf(os.Stderr, "traceless: %s: %s\n", safe(path), safe(err.Error()))
	return exitFail
}

// maxMarks is how many combining marks in a row safe lets through. Real text
// rarely stacks more; long stacks are used to hide or smear text.
const maxMarks = 4

// safe escapes characters that could control the terminal (escape sequences,
// bidi overrides, newlines) or make text look different from what it is
// (invisible characters, stacked combining marks). File names and metadata
// are attacker-controlled.
func safe(s string) string {
	var b strings.Builder
	marks := 0
	for i, r := range s {
		isMark := unicode.In(r, unicode.Mn, unicode.Me)
		if isMark {
			marks++
		} else {
			marks = 0
		}
		switch {
		case visible(r) && !(isMark && (i == 0 || marks > maxMarks)):
			b.WriteRune(r)
		case r <= 0xffff:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	return b.String()
}

// visible reports whether r prints as something a reader can see.
func visible(r rune) bool {
	const brailleBlank = 0x2800
	return unicode.IsGraphic(r) &&
		!unicode.In(r, unicode.Variation_Selector, unicode.Other_Default_Ignorable_Code_Point) &&
		r != brailleBlank
}
