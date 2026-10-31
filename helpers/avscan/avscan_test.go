package avscan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
)

type fixedScanner struct{ v Verdict }

func (f fixedScanner) Scan(context.Context, io.Reader) (Result, error) {
	return Result{Verdict: f.v}, nil
}

type otherScanner struct{}

func (otherScanner) Scan(context.Context, io.Reader) (Result, error) { return Result{}, nil }

// SetDefault(nil) turns scanning off, as its comment says, and one scanner can
// replace another of a different type. Both panicked: atomic.Value refuses a
// nil value and a value of another concrete type than the first it held.
func TestTheDefaultScannerCanBeSwappedAndTurnedOff(t *testing.T) {
	t.Cleanup(func() { SetDefault(nil) })
	SetDefault(fixedScanner{v: VerdictInfected})
	if s := Default(); s == nil {
		t.Fatal("no scanner after setting one")
	}
	SetDefault(otherScanner{})
	if _, ok := Default().(otherScanner); !ok {
		t.Errorf("the scanner is %T after swapping in another kind", Default())
	}
	SetDefault(nil)
	if s := Default(); s != nil {
		t.Errorf("scanning is still on: %T", s)
	}
	SetDefault(fixedScanner{})
	if s := Default(); s == nil {
		t.Error("no scanner after turning it on again")
	}
}

// An upload refused for a virus names it, and is still ErrInfected to the
// handlers that only ask whether it was refused.
func TestARefusalNamesTheSignatureItFound(t *testing.T) {
	err := fmt.Errorf("upload blocked: %w", &Infected{Signature: "Eicar-Test-Signature"})
	var infected *Infected
	if !errors.Is(err, ErrInfected) || !errors.As(err, &infected) || infected.Signature != "Eicar-Test-Signature" {
		t.Fatalf("%v doesn't say it's infected with Eicar", err)
	}
	if err.Error() != "upload blocked: the file carries a virus signature (Eicar-Test-Signature)" {
		t.Fatalf("it reads %q", err)
	}
}
