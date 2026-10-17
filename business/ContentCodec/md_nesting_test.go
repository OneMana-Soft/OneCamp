package contentcodec

import (
	"errors"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
)

// Markdown whose quotes or lists nest on one line past helpers.MaxMarkdownNesting is
// refused before the parser reads it: the parser's time grows faster than the
// nesting (a line of 500,000 ">" took six minutes and half a gigabyte of
// stack), and the public API takes a megabyte. Up to the limit it reads as it
// always has.
func TestMarkdownNestedPastTheLimitIsRefusedBeforeItIsRead(t *testing.T) {
	over := strings.Repeat(">", helpers.MaxMarkdownNesting+1) + " deep\n"
	if doc, err := FromMarkdown(over, nil); !errors.Is(err, ErrTooDeep) || doc != nil {
		t.Fatalf("%d quotes deep: %v, %v (want ErrTooDeep)", helpers.MaxMarkdownNesting+1, doc, err)
	}
	// A megabyte on one line, as the API takes it: refused, not parsed.
	if _, err := FromMarkdown(strings.Repeat(">", 1<<20)+" x", nil); !errors.Is(err, ErrTooDeep) {
		t.Fatalf("a megabyte of quotes: %v", err)
	}
	// Mixed quotes and list items count alike.
	if _, err := FromMarkdown(strings.Repeat("> - ", helpers.MaxMarkdownNesting/2+1)+"x", nil); !errors.Is(err, ErrTooDeep) {
		t.Fatalf("quotes and lists past the limit: %v", err)
	}

	at := strings.Repeat(">", helpers.MaxMarkdownNesting) + " deep\n"
	doc, err := FromMarkdown(at, nil)
	if err != nil {
		t.Fatalf("%d quotes deep: %v", helpers.MaxMarkdownNesting, err)
	}
	depth := 0
	for n := doc; n != nil && len(n.Content) > 0 && n.Content[0].Type == "blockquote"; n = n.Content[0] {
		depth++
	}
	if depth != helpers.MaxMarkdownNesting {
		t.Fatalf("read %d quotes deep, want %d", depth, helpers.MaxMarkdownNesting)
	}
}
