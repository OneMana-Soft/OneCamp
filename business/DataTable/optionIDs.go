package business

// Option ids made permanent (tables A1, design/tables-vs-airtable/PROGRAM.md
// §6): an option saved before options had ids is known by its label as
// "label:<label>" (selectOptions.go), here and on the web, until its field is
// next saved. This one-time job saves every such field's ids as they're
// already known, so an option's id is stored for good and never derived from
// a label again. No cell changes: the ids are the ones every reader derives.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	"github.com/akashc777/OneCamp/helpers"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// optionIDsKey records in the system config that every option has its id
// stored (KeepOptionIDs).
const optionIDsKey = "tables_option_ids_permanent"

// optionIDsPage is how many fields one pass of the job reads at a time.
const optionIDsPage = 200

// KeepOptionIDs stores the derived id of every option stored without one,
// once per install, and answers how many fields it saved. A field saved by
// someone while the job planned it is left for the next pass (the save is
// conditional on the config the job read), which finds it with its ids.
func KeepOptionIDs(ctx context.Context) (int, error) {
	if done, err := configModel.IsDone(optionIDsKey); err != nil || done {
		return 0, err
	}
	saved, missed := 0, 0
	for after := uuid.Nil; ; {
		fields, err := model.OptionFieldsWithoutIDs(ctx, after, optionIDsPage)
		if err != nil {
			return saved, err
		}
		if len(fields) == 0 {
			break
		}
		for _, f := range fields {
			after = f.Id
			cfg := map[string]interface{}{}
			if json.Unmarshal([]byte(f.Config), &cfg) != nil {
				continue
			}
			// The options against themselves: each keeps its label, and gets
			// the id it's known by.
			planOptions(optionsOf(cfg), cfg, nil, false)
			next, err := json.Marshal(cfg)
			if err != nil {
				continue
			}
			ok, err := model.SetFieldConfigIf(ctx, f.Id, f.Config, string(next))
			if err != nil {
				return saved, err
			}
			if ok {
				saved++
			} else {
				missed++
			}
		}
	}
	if missed > 0 {
		return saved, nil // a pass after the saves meanwhile finishes the job
	}
	return saved, configModel.UpsertConfig(optionIDsKey, fmt.Sprintf("done: %d fields", saved))
}

// optionsOf is a config's options as planOptions reads stored ones: a copy,
// so planning their ids doesn't change what it compares them with.
func optionsOf(cfg map[string]interface{}) []map[string]interface{} {
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil
	}
	var copied map[string]interface{}
	if json.Unmarshal(b, &copied) != nil {
		return nil
	}
	return fieldTypes.Options(copied)
}

func init() {
	// Hourly, claimed by one worker at a time, until it's done once.
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "tables.option-ids", Every: time.Hour, Run: func(ctx context.Context) error {
		saved, err := KeepOptionIDs(ctx)
		if saved > 0 {
			helpers.MessageLogs.InfoLog.Printf("tables: the option ids of %d fields stored for good", saved)
		}
		return err
	}})
}
