package domain

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	opensearchModels "github.com/akashc777/OneCamp/models/openSearch/GlobalSearch"
)

func SyncCascadingDeletionInOpenSearch(parentFields []string, parentID string, deletedAt int64, indices []string, deletedBy string) (err error) {
	err = opensearchModels.SyncCascadingDeletionInOpenSearch(context.Background(), parentFields, parentID, deletedAt, indices, deletedBy)
	if err != nil {
		helpers.LogErrorWithContext(context.Background(),
			"domain/SyncCascadingDeletionInOpenSearch Failed to cascade deletion err: %+v",
			err)
		return
	}

	return
}

// HideInOpenSearch marks hidden by hiddenBy the entries in indices that name
// id in any of fields, a cascade's mark included, never a person's
// deletion; SyncCascadingUnarchiveInOpenSearch with hiddenBy brings them back.
func HideInOpenSearch(fields []string, id string, hiddenAt int64, indices []string, hiddenBy string) error {
	return opensearchModels.HideInOpenSearch(context.Background(), fields, id, hiddenAt, indices, hiddenBy)
}

// HandOverInOpenSearch passes the entries in indices that name id in any of
// fields, and that from marked, to the marker to at at, so to's restore
// brings them back.
func HandOverInOpenSearch(fields []string, id, from string, at int64, indices []string, to string) error {
	return opensearchModels.HandOverInOpenSearch(context.Background(), fields, id, from, at, indices, to)
}

func SyncCascadingUnarchiveInOpenSearch(parentFields []string, parentID string, indices []string, deletedBy string) (err error) {
	err = opensearchModels.SyncCascadingUnarchiveInOpenSearch(context.Background(), parentFields, parentID, indices, deletedBy)
	if err != nil {
		helpers.LogErrorWithContext(context.Background(),
			"domain/SyncCascadingUnarchiveInOpenSearch Failed to cascade unarchive err: %+v",
			err)
		return
	}

	return
}

func SyncCascadingDeletionInOpenSearchMulti(parentField string, parentIDs []string, deletedAt int64, indices []string, deletedBy string) {
	opensearchModels.SyncCascadingDeletionInOpenSearchMulti(context.Background(), parentField, parentIDs, deletedAt, indices, deletedBy)
}

func SyncCascadingUnarchiveInOpenSearchMulti(parentField string, parentIDs []string, indices []string, deletedBy string) {
	opensearchModels.SyncCascadingUnarchiveInOpenSearchMulti(context.Background(), parentField, parentIDs, indices, deletedBy)
}

func SyncCascadingDeletionInOpenSearchCombined(fieldIDs map[string][]string, deletedAt int64, indices []string, deletedBy string) {
	opensearchModels.SyncCascadingDeletionInOpenSearchCombined(context.Background(), fieldIDs, deletedAt, indices, deletedBy)
}

func SyncCascadingUnarchiveInOpenSearchCombined(fieldIDs map[string][]string, indices []string, deletedBy string) {
	opensearchModels.SyncCascadingUnarchiveInOpenSearchCombined(context.Background(), fieldIDs, indices, deletedBy)
}

// MarkChannelArchivedInOpenSearch marks a channel's posts, replies and files
// as in an archived channel, or clears the mark (models).
func MarkChannelArchivedInOpenSearch(channelUUID string, archived bool) {
	_ = opensearchModels.MarkChannelArchivedInOpenSearch(context.Background(), channelUUID, archived)
}
