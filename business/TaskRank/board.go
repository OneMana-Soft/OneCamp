package taskrank

import (
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// Pick orders each column of a board read as places
// (dgraphStruct.BoardPlacesQuery) and picks the cards it shows: an open
// column's first openLimit of each part (Window, with partOf naming the part
// a task shows in), a closed column whole, as it was read as its newest
// closedLimit. It returns the picks by column key, still places, and every
// pick's uid for reading their cards; Fill puts the cards in.
func Pick(columns map[string]*dgraphStruct.BoardPlaces, openLimit int, partOf func(*dgraphStruct.DgraphTask) string) (picks map[string][]*dgraphStruct.DgraphTask, uids []string) {
	picks = make(map[string][]*dgraphStruct.DgraphTask, len(dgraphStruct.BoardColumns))
	for _, c := range dgraphStruct.BoardColumns {
		col := columns[c.Key]
		if col == nil {
			continue
		}
		var picked []*dgraphStruct.DgraphTask
		if c.Closed {
			Sort(col.Places)
			picked = col.Places
		} else {
			picked = Window(col.Places, openLimit, partOf)
		}
		picks[c.Key] = picked
		for _, t := range picked {
			uids = append(uids, t.Uid)
		}
	}
	return picks, uids
}

// Fill swaps each pick for its card, matched by uuid, keeping the board's
// order; a pick whose card wasn't read (deleted since) is left out.
func Fill(picks map[string][]*dgraphStruct.DgraphTask, cards []*dgraphStruct.DgraphTask) map[string][]*dgraphStruct.DgraphTask {
	byUUID := make(map[string]*dgraphStruct.DgraphTask, len(cards))
	for _, c := range cards {
		if c != nil {
			byUUID[c.Uuid] = c
		}
	}
	out := make(map[string][]*dgraphStruct.DgraphTask, len(picks))
	for key, column := range picks {
		var filled []*dgraphStruct.DgraphTask
		for _, p := range column {
			if card := byUUID[p.Uuid]; card != nil {
				filled = append(filled, card)
			}
		}
		out[key] = filled
	}
	return out
}
