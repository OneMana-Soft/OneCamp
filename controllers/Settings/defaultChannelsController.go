package controllers

// Where new members start: the admin's choice of the channels everyone who
// joins is put in (business/Channel.JoinDefaultChannels).

import (
	"encoding/json"
	"errors"
	"net/http"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	"github.com/akashc777/OneCamp/helpers"
)

// defaultChannelsView is what the admin screen shows: the channels new members
// join, whether an admin chose them (otherwise it is #general), and the public
// channels there are to choose from.
func defaultChannelsView(r *http.Request) (helpers.Envolope, error) {
	ctx := r.Context()
	current, chosen, err := channelBusiness.DefaultChannels(ctx)
	if err != nil {
		return nil, err
	}
	available, err := channelBusiness.JoinableChannels(ctx)
	if err != nil {
		return nil, err
	}
	return helpers.Envolope{"data": map[string]interface{}{
		"channels":  current,
		"chosen":    chosen,
		"available": available,
	}}, nil
}

// GetDefaultChannels handles GET /admin/default-channels.
func GetDefaultChannels(w http.ResponseWriter, r *http.Request) {
	view, err := defaultChannelsView(r)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/GetDefaultChannels err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Couldn't read the channels new members join."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, view)
}

// SetDefaultChannels handles POST /admin/default-channels {channel_uuids}:
// the channels new members join, which must be public and not archived. An
// empty list means none.
func SetDefaultChannels(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChannelUUIDs []string `json:"channel_uuids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil || body.ChannelUUIDs == nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say which channels new members join."})
		return
	}
	channels, err := channelBusiness.SetDefaultChannels(r.Context(), body.ChannelUUIDs)
	if err != nil {
		if errors.Is(err, channelBusiness.ErrNotAChannel) || errors.Is(err, channelBusiness.ErrNotJoinable) ||
			errors.Is(err, channelBusiness.ErrTooManyDefaultChannels) {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
			return
		}
		helpers.LogErrorWithContext(r.Context(), "controllers/SetDefaultChannels err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Couldn't save the channels new members join."})
		return
	}
	names := make([]string, 0, len(channels))
	for _, ch := range channels {
		names = append(names, "#"+ch.Name)
	}
	summary := "New members join no channel"
	if len(names) > 0 {
		summary = "Changed the channels new members join"
	}
	auditBusiness.Record(r, "settings.default_channels", auditBusiness.CategorySettings, summary,
		map[string]interface{}{"channels": names})

	view, err := defaultChannelsView(r)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/SetDefaultChannels read back err: %+v", err)
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{"channels": channels, "chosen": true}})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, view)
}
