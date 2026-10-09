package business

// Two accounts at one address.
//
// Accounts are found by address without regard to case now
// (helpers.NormalizeEmail), and an install from before that can hold two
// whose addresses differ only in capital letters, Ana@ and ana@, made by two
// different ways in. An address is lowercased before it is looked up, so a
// sign-in reaches the account whose address is all lowercase, when there is
// one, and otherwise the older; the other can't be reached by its address at
// all. Nothing can merge them safely on its own, so the admin's system check
// names them, and a person decides.

import (
	"context"
	"fmt"
	"strings"

	domain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
)

// sharedAddressesShown bounds the list in the note; the rest are counted by
// the admin when they look.
const sharedAddressesShown = 5

func init() {
	helpers.RegisterSystemCheck(helpers.SystemCheck{
		Name: "accounts",
		Describe: "No two accounts have the same address apart from capital letters, so every sign-in " +
			"reaches one account. It does not check that each address is its owner's.",
		Probe: func(ctx context.Context) error {
			shared, err := domain.AddressesWithMoreThanOneAccount(ctx, sharedAddressesShown)
			if err != nil {
				return err
			}
			return sharedAddressesNote(shared)
		},
	})
}

// sharedAddressesNote is what the check says about these addresses: nothing
// for none, otherwise a note naming them and what to do. A note, not a
// failure: every account still works. Pure.
func sharedAddressesNote(shared []string) error {
	if len(shared) == 0 {
		return nil
	}
	return helpers.SystemCheckNote(fmt.Sprintf(
		"%s each belong to more than one account, spelt with different capital letters. Signing in "+
			"reaches the account whose address is all lowercase, or else the older one; the other can't be "+
			"signed in to. Deactivate the account nobody uses.",
		strings.Join(shared, ", ")))
}
