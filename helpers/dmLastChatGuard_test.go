package helpers

import (
	"go/ast"
	"path/filepath"
	"strings"
	"testing"
)

// Every write that adds messages to a DM moves the DM's last time in the same
// request (DG4): the DM list pages by it, so a DM a write forgot would sit
// out of order, or off the list. The two graph writes that carry a DM's
// messages both stamp it (models/dgraph StampLastChats).
//
// dmChatWrites is every function that builds a DM with messages (a DgraphDm
// literal with Chats) and the write it hands the DM to. A new one fails this
// until it is listed here with a write that stamps.
var dmChatWrites = map[string]string{
	"business/Chat/chatBusiness.go writeDMNode":                                  "CreateDgraphChat", // a send to a person, a bot's, an import's
	"business/Chat/chatBusiness.go writeGroupNode":                               "CreateDgraphChat", // a send to a group, a bot's, an import's
	"business/BulkPostAndChat/bulkPostAndChatBusiness.go BulkPostAndChatForward": "BulkAddChatAndPostToDgraph",
	"business/Conversation/share.go writeMessage":                                "CreateDgraphChat", // a shared history's copy
	"business/Conversation/systemLine.go writeGroupLine":                         "writeChatNode",    // = CreateDgraphChat
}

// stampingWrites are the model functions under those writes, each of which
// must call StampLastChats.
var stampingWrites = map[string]bool{
	"models/dgraph/Chat/chatModel.go CreateChat":                                       true,
	"models/dgraph/BulkPostAndChat/bulkPostAndChatModel.go BulkAddChatAndPostToDgraph": true,
}

func TestEveryWriteOfADMsMessagesStampsItsLastTime(t *testing.T) {
	found := map[string]bool{}
	stamped := map[string]bool{}
	goFuncs(t, []string{"../business", "../domain", "../models", "../controllers", "../services"}, nil, func(path string, fd *ast.FuncDecl) {
		key := strings.TrimPrefix(filepath.ToSlash(path), "../") + " " + fd.Name.Name
		calls := map[string]bool{}
		buildsDMChats := false
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CallExpr:
				switch f := v.Fun.(type) {
				case *ast.Ident:
					calls[f.Name] = true
				case *ast.SelectorExpr:
					calls[f.Sel.Name] = true
				}
			case *ast.CompositeLit:
				if se, ok := v.Type.(*ast.SelectorExpr); ok && se.Sel.Name == "DgraphDm" {
					for _, e := range v.Elts {
						if kv, ok := e.(*ast.KeyValueExpr); ok {
							if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Chats" {
								buildsDMChats = true
							}
						}
					}
				}
			}
			return true
		})
		if buildsDMChats {
			found[key] = true
			want, listed := dmChatWrites[key]
			switch {
			case !listed:
				t.Errorf("%s builds a DM with messages and isn't in dmChatWrites: hand it to a write that stamps the DM's last time, and list it", key)
			case !calls[want]:
				t.Errorf("%s no longer calls %s, the write dmChatWrites names for it", key, want)
			}
		}
		if stampingWrites[key] {
			stamped[key] = calls["StampLastChats"]
		}
	})
	for key := range dmChatWrites {
		if !found[key] {
			t.Errorf("dmChatWrites lists %s, which builds no DM with messages now: take it out", key)
		}
	}
	for key := range stampingWrites {
		if !stamped[key] {
			t.Errorf("%s writes DMs' messages without StampLastChats", key)
		}
	}
}
