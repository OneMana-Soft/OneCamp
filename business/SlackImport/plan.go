package business

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	importAdapter "github.com/akashc777/OneCamp/adapter/SlackImport"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	"github.com/google/uuid"
)

// BuildPlan opens the staged ZIP, parses manifests, walks per-day files
// to count messages/threads/files, and stores the resulting plan on the
// job row. It also creates the chunk queue (one chunk per per-day file
// for messages, one per file for files) so Run can start workers
// immediately.
//
// This pass is allowed to be slow (10–60s on multi-GB exports). It runs
// inline on the controller goroutine so the operator gets a clear error
// if the zip is malformed. We bound it with the request context.
func BuildPlan(ctx context.Context, jobId uuid.UUID, arc *Archive) (*importAdapter.PlanResponse, error) {
	parsed, err := ParseManifests(arc.Reader)
	if err != nil {
		return nil, err
	}

	resp := &importAdapter.PlanResponse{
		JobId:        jobId,
		UserCount:    len(parsed.Users),
		ChannelCount: len(parsed.Channels) + len(parsed.Groups),
	}

	// Pre-compute user resolution outcomes against the live OneCamp DB.
	// We don't write anything; just count.
	for _, u := range parsed.Users {
		email := strings.TrimSpace(strings.ToLower(u.Profile.Email))
		if email == "" {
			resp.UserNew++
			continue
		}
		if existing, _ := userBusiness.GetUserByEmailId(ctx, &email); existing != nil {
			resp.UserMerge++
		} else {
			resp.UserNew++
		}
	}

	// Pre-compute channel name conflicts. Cover both public and private.
	for _, c := range parsed.Channels {
		desired := sanitizeChannelName(strings.ToLower(c.Name))
		if exist, _ := channelBusiness.CheckIfChannelExist(ctx, &desired); exist {
			resp.ChannelConflict++
			resp.Warnings = append(resp.Warnings,
				fmt.Sprintf("Channel %q exists; will be imported as %q-from-slack", c.Name, desired))
		}
	}
	for _, c := range parsed.Groups {
		desired := sanitizeChannelName(strings.ToLower(c.Name))
		if exist, _ := channelBusiness.CheckIfChannelExist(ctx, &desired); exist {
			resp.ChannelConflict++
			resp.Warnings = append(resp.Warnings,
				fmt.Sprintf("Private channel %q exists; will be imported as %q-from-slack", c.Name, desired))
		}
	}

	// Walk message files to count messages, threads, and files.
	chunks := make([]*importModels.Chunk, 0, 64)
	totalMessages := 0
	totalThreads := 0
	totalFiles := 0
	totalFileBytes := int64(0)

	for slackChannelId, dailyFiles := range parsed.MessageFiles {
		if len(dailyFiles) == 0 {
			continue
		}
		// Three categories of conversation:
		//   1. Public channel or private group — written via posts.
		//   2. DM (1:1) or MPIM (multi-party) — written via chats.
		//   3. Anything else — skip with a warning.
		isChannelLike := isImportableChannelId(parsed, slackChannelId)
		isDM := isDMId(parsed, slackChannelId)
		isMPIM := isMPIMId(parsed, slackChannelId)
		if !isChannelLike && !isDM && !isMPIM {
			resp.Warnings = append(resp.Warnings,
				fmt.Sprintf("Unknown conversation %s skipped (not in channels.json, groups.json, dms.json, or mpims.json)", slackChannelId))
			continue
		}

		// Schedule one chunk per daily file for messages (top-level pass).
		for _, dailyPath := range dailyFiles {
			zf, err := OpenInZip(arc.Reader, dailyPath)
			if err != nil {
				continue
			}
			// Count messages and files in this daily.
			perFileMessages := 0
			perFileThreads := 0
			err = IterMessages(ctx, arc, zf, func(m *SlackMessage) bool {
				perFileMessages++
				if m.ThreadTs != "" && m.ThreadTs != m.Ts {
					perFileThreads++
				}
				for _, f := range m.Files {
					if f.URLPrivateDownload != "" || f.URLPrivate != "" {
						totalFiles++
						totalFileBytes += f.Size
					}
				}
				return true
			})
			if err != nil {
				helpers.LogErrorWithContext(ctx,
					"SlackImport.BuildPlan failed to count %s: %+v", dailyPath, err)
				resp.Warnings = append(resp.Warnings,
					fmt.Sprintf("could not read %s: %v", dailyPath, err))
				continue
			}
			totalMessages += perFileMessages
			totalThreads += perFileThreads

			// Pick chunk type based on conversation kind. The DM/MPIM
			// pipeline writes chats; the channel pipeline writes posts.
			topLevelType := importModels.ChunkChannelMessages
			threadsType := importModels.ChunkChannelThreads
			if isDM || isMPIM {
				// DMs/MPIMs route both top-level and thread replies to
				// the dm worker because thread replies in chats land as
				// chat-comments via the same conversation grouping; the
				// channel-threads worker doesn't know about DM groupings.
				// processDMChunk does an internal two-pass over the same
				// daily file (top-level then replies), so we don't need
				// a separate threads chunk for DMs.
				topLevelType = importModels.ChunkDMMessages
				threadsType = ""
			}

			// Top-level pass chunk.
			perFileTotal := perFileMessages
			chunkId := uuid.New()
			chunks = append(chunks, &importModels.Chunk{
				Id:             chunkId,
				ImportId:       jobId,
				ChunkType:      topLevelType,
				ChannelSlackId: ptrStr(slackChannelId),
				ObjectKey:      ptrStr(dailyPath),
				Status:         importModels.ChunkStatusPending,
				MaxAttempts:    5,
				ItemsTotal:     &perFileTotal,
			})

			// Threads pass chunk — only for channel-like conversations.
			// DM/MPIM threads are handled inline by the DM worker so we
			// don't double-up here.
			if threadsType != "" && perFileThreads > 0 {
				chunkId = uuid.New()
				chunks = append(chunks, &importModels.Chunk{
					Id:             chunkId,
					ImportId:       jobId,
					ChunkType:      threadsType,
					ChannelSlackId: ptrStr(slackChannelId),
					ObjectKey:      ptrStr(dailyPath),
					Status:         importModels.ChunkStatusPending,
					MaxAttempts:    5,
					ItemsTotal:     &perFileThreads,
				})
			}
		}
	}

	// One chunk per file URL. Files chunks live behind a separate worker
	// pool so they don't block message imports.
	if totalFiles > 0 {
		// Files are scheduled lazily by the message worker (it knows the
		// concrete file id and url after parsing each message). We don't
		// pre-enqueue here to avoid duplicating walk effort.
	}

	// Single reaction-pass chunk that runs after messages+threads.
	chunks = append(chunks, &importModels.Chunk{
		Id:          uuid.New(),
		ImportId:    jobId,
		ChunkType:   importModels.ChunkReactionPass,
		Status:      importModels.ChunkStatusPending,
		MaxAttempts: 3,
	})

	resp.MessageCount = totalMessages
	resp.ThreadCount = totalThreads
	resp.FileCount = totalFiles
	resp.FileBytes = totalFileBytes

	warnIfFileLinksHaveExpired(resp, parsed, time.Now())

	if err := importModels.CreateChunks(ctx, chunks); err != nil {
		return nil, fmt.Errorf("create chunks: %w", err)
	}

	planJSON, _ := json.Marshal(resp)
	if err := importModels.UpdatePlan(ctx, jobId, planJSON); err != nil {
		return nil, err
	}
	return resp, nil
}

// exportDayPattern matches the per-day message files Slack writes as
// "<channel>/<YYYY-MM-DD>.json".
var exportDayPattern = regexp.MustCompile(`(\d{4})-(\d{2})-(\d{2})\.json$`)

// newestExportDay returns the most recent day present in the archive.
//
// Slack stamps no export date inside the zip, but an export contains messages
// up to the day it was taken, so the newest per-day file is that date to within
// a day. Good enough for a warning and requires no extra pass: these are the
// same paths the plan already walked.
func newestExportDay(messageFiles map[string][]string) (time.Time, bool) {
	var newest time.Time
	for _, paths := range messageFiles {
		for _, p := range paths {
			m := exportDayPattern.FindStringSubmatch(p)
			if m == nil {
				continue
			}
			day, err := time.Parse("2006-01-02", m[1]+"-"+m[2]+"-"+m[3])
			if err != nil {
				continue
			}
			if day.After(newest) {
				newest = day
			}
		}
	}
	return newest, !newest.IsZero()
}

// warnIfFileLinksHaveExpired tells the operator when an export is old enough
// that its attachments will not come across.
//
// This is the most expensive silence in the product. Slack exports carry file
// URLs rather than file bytes, and those URLs stop working roughly ninety days
// after the export. Someone who exported in March and imports in July gets every
// message and no attachments, the run finishes green, and the files are gone
// from a Slack they have already left. The importer knew the clock existed; the
// person did not.
func warnIfFileLinksHaveExpired(resp *importAdapter.PlanResponse, parsed *ParsedExport, now time.Time) {
	if resp.FileCount == 0 {
		return
	}
	day, ok := newestExportDay(parsed.MessageFiles)
	if !ok {
		return
	}

	age := now.Sub(day)
	switch {
	case age > SlackFileLinkWindow:
		resp.Warnings = append(resp.Warnings, fmt.Sprintf(
			"This export is about %d days old and Slack's file links expire after roughly %d. "+
				"The %d attachments will probably fail to download. Take a fresh export from Slack "+
				"before running this, or import now and accept that files may be missing.",
			int(age.Hours()/24), int(SlackFileLinkWindow.Hours()/24), resp.FileCount))
	case age > SlackFileLinkWindow-14*24*time.Hour:
		resp.Warnings = append(resp.Warnings, fmt.Sprintf(
			"This export is about %d days old. Slack's file links expire after roughly %d, "+
				"so run this soon or the %d attachments may not come across.",
			int(age.Hours()/24), int(SlackFileLinkWindow.Hours()/24), resp.FileCount))
	}
}

// isPublicChannelId reports whether id is a Slack public channel.
// Public channels imported as OneCamp ChannelPrivate=false.
func isPublicChannelId(p *ParsedExport, id string) bool {
	for _, c := range p.Channels {
		if c.ID == id {
			return true
		}
	}
	return false
}

// isPrivateGroupId reports whether id is a Slack "group" (private channel).
// These are imported as OneCamp ChannelPrivate=true.
func isPrivateGroupId(p *ParsedExport, id string) bool {
	for _, c := range p.Groups {
		if c.ID == id {
			return true
		}
	}
	return false
}

// isDMId returns true if id is in dms.json.
func isDMId(p *ParsedExport, id string) bool {
	for _, c := range p.DMs {
		if c.ID == id {
			return true
		}
	}
	return false
}

// isMPIMId returns true if id is in mpims.json.
func isMPIMId(p *ParsedExport, id string) bool {
	for _, c := range p.MPIMs {
		if c.ID == id {
			return true
		}
	}
	return false
}

// isImportableChannelId returns true if the id is a public channel or a
// private group — anything we'll create a OneCamp Channel for. DMs and
// MPIMs are NOT importable channels (they go via resolveDMs).
func isImportableChannelId(p *ParsedExport, id string) bool {
	return isPublicChannelId(p, id) || isPrivateGroupId(p, id)
}

func ptrStr(s string) *string { return &s }
