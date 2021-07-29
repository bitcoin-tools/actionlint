package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/format"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

var dbg = log.New(ioutil.Discard, "", log.LstdFlags)

type webhook struct {
	name  string
	types []string
}

func getFirstLinkText(n ast.Node, src []byte) (string, bool) {
	var link ast.Node
	ast.Walk(n, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkStop, nil
		}

		if n.Kind() != ast.KindLink {
			return ast.WalkContinue, nil
		}

		link = n // Found
		return ast.WalkStop, nil
	})

	if link == nil {
		return "", false
	}

	// Note: All text pieces must be collected. For example the text "pull_request" is pieces of
	// "pull_" and "request" since an underscore is delimiter of italic/bold text.
	var b strings.Builder
	for c := link.FirstChild(); c != nil; c = c.NextSibling() {
		b.Write(c.Text(src))
	}

	return b.String(), true
}

func collectCodeSpans(n ast.Node, src []byte) []string {
	spans := []string{}
	ast.Walk(n, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		kind := n.Kind()
		if entering && kind == ast.KindCodeSpan {
			spans = append(spans, string(n.Text(src)))
		}
		return ast.WalkContinue, nil
	})
	return spans
}

func getWebhookInTable(table ast.Node, src []byte) (*webhook, bool) {
	dbg.Printf("Table: %s", table.Text(src))

	sawHeader := false
	for c := table.FirstChild(); c != nil; c = c.NextSibling() {
		kind := c.Kind()

		if kind == extast.KindTableHeader {
			sawHeader = true

			cell := c.FirstChild()
			if string(cell.Text(src)) != "Webhook event payload" {
				dbg.Println("  Skip this table because it is not for Webhook event payload")
				return nil, false
			}

			dbg.Println("  Found table header for Webhook event payload")
			continue
		}

		if kind == extast.KindTableRow {
			if !sawHeader {
				// Without header or on second row
				dbg.Println("  Skip this table because it does not have a header")
				return nil, false
			}

			dbg.Println("  Found the first table row")

			// First cell of first row
			cell := c.FirstChild()
			name, ok := getFirstLinkText(cell, src)
			if !ok {
				dbg.Printf("  Skip this table because it does not have a link at the first cell of the first row: %s", cell.Text(src))
				return nil, false
			}

			// Second cell
			cell = cell.NextSibling()
			types := collectCodeSpans(cell, src)

			dbg.Printf("  Found Webhook table: %q %v", name, types)
			w := &webhook{name, types}
			return w, true
		}
	}

	dbg.Printf("  Table row was not found (sawHeader=%v)", sawHeader)
	return nil, false
}

func generate(src []byte, out io.Writer) error {
	md := goldmark.New(goldmark.WithExtensions(extension.Table))
	root := md.Parser().Parse(text.NewReader(src))

	buf := &bytes.Buffer{}
	fmt.Fprintln(buf, `// Code generated by actionlint/scripts/generate-webhook-events. DO NOT EDIT.

package actionlint

// AllWebhookTypes is a table of all webhooks with their types. This variable was generated by
// script at ./scripts/generate-webhook-events based on
// https://github.com/github/docs/blob/main/content/actions/reference/events-that-trigger-workflows.md .
var AllWebhookTypes = map[string][]string {`)

	numHooks := 0
	sawHeading := false
	for n := root.FirstChild(); n != nil; n = n.NextSibling() {
		k := n.Kind()
		if !sawHeading {
			// When '## Webhook events'
			if h, ok := n.(*ast.Heading); ok && h.Level == 2 && "Webhook events" == string(h.Text(src)) {
				sawHeading = true
				dbg.Println("Found \"Webhook events\" heading")
			}
			continue
		}

		if k != extast.KindTable {
			continue
		}

		w, ok := getWebhookInTable(n, src)
		if !ok {
			continue
		}
		numHooks++

		if len(w.types) == 0 {
			fmt.Fprintf(buf, "\t%q: {},\n", w.name)
			continue
		}
		fmt.Fprintf(buf, "\t%q: {%q", w.name, w.types[0])
		for _, t := range w.types[1:] {
			fmt.Fprintf(buf, ", %q", t)
		}
		fmt.Fprintln(buf, "},")
	}
	fmt.Fprintln(buf, "}")

	if !sawHeading {
		return errors.New("\"## Webhook events\" heading was missing")
	}

	if numHooks == 0 {
		return errors.New("no webhook table was found in given markdown source")
	}

	src, err := format.Source(buf.Bytes())
	if err != nil {
		return fmt.Errorf("could not format Go source: %w", err)
	}

	if _, err := out.Write(src); err != nil {
		return fmt.Errorf("could not write output: %w", err)
	}

	return nil
}

var srcURL = "https://raw.githubusercontent.com/github/docs/main/content/actions/reference/events-that-trigger-workflows.md"

func fetchMarkdownSource() ([]byte, error) {
	var c http.Client

	dbg.Println("Fetching", srcURL)

	res, err := c.Get(srcURL)
	if err != nil {
		return nil, fmt.Errorf("could not fetch %s: %w", srcURL, err)
	}
	if res.StatusCode < 200 || 300 <= res.StatusCode {
		return nil, fmt.Errorf("request was not successful for %s: %s", srcURL, res.Status)
	}
	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("could not fetch body for %s: %w", srcURL, err)
	}
	res.Body.Close()

	dbg.Printf("Fetched %d bytes from %s", len(body), srcURL)
	return body, nil
}

func run(args []string, stdout, stderr, dbgout io.Writer) int {
	dbg.SetOutput(dbgout)

	if len(args) > 2 {
		fmt.Fprintln(stderr, "usage: generate-webhook-events events-that-trigger-workflows.md [[srcfile] dstfile]")
		return 1
	}

	var src []byte
	var err error
	if len(args) == 2 {
		src, err = ioutil.ReadFile(args[0])
	} else {
		src, err = fetchMarkdownSource()
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	var out io.Writer
	var dst string
	if len(args) == 0 || args[len(args)-1] == "-" {
		out = stdout
		dst = "stdout"
	} else {
		n := args[len(args)-1]
		f, err := os.Create(n)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		defer f.Close()
		out = f
		dst = n
	}

	if err := generate(src, out); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	dbg.Println("Successfully wrote output to", dst)
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Stderr))
}
