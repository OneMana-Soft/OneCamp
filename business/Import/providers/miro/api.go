package miro

// The parts of Miro's REST API v2 answers the import reads, as its public
// OpenAPI file (github.com/miroapp/api-clients, packages/generator/spec.json,
// read 21 Oct 2026) describes them. Fields not listed are not read.

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// id is a Miro id. The spec writes them as strings ("3458764517517852417"),
// some as int64 strings; a number is read as its digits too.
type id string

func (i *id) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*i = ""
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*i = id(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*i = id(n.String())
	return nil
}

// num is a number Miro writes as a string ("2.0", "14") or as a number.
type num float64

func (n *num) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	s := string(b)
	if b[0] == '"' {
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), "%")
	if s == "" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil // a value it can't read is left as nothing, never the whole answer refused
	}
	*n = num(v)
	return nil
}

// board is one board (Board).
type board struct {
	ID          id     `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	ViewLink    string `json:"viewLink"`
	ModifiedAt  string `json:"modifiedAt"`
	Owner       *struct {
		Name string `json:"name"`
	} `json:"owner"`
}

// boardsPage is GET /v2/boards (BoardsPagedResponse): offset paging.
type boardsPage struct {
	Data   []board `json:"data"`
	Total  int     `json:"total"`
	Size   int     `json:"size"`
	Offset int     `json:"offset"`
}

// position is an item's centre (origin "center"), relative to the canvas's
// centre or to its parent's top left (relativeTo).
type position struct {
	X          num    `json:"x"`
	Y          num    `json:"y"`
	Origin     string `json:"origin"`
	RelativeTo string `json:"relativeTo"`
}

// geometry is an item's size, and its rotation in degrees.
type geometry struct {
	Width    num `json:"width"`
	Height   num `json:"height"`
	Rotation num `json:"rotation"`
}

// item is one item on a board (GenericItem; the typed items add style). Its
// data and style are read by its type.
type item struct {
	ID       id              `json:"id"`
	Type     string          `json:"type"`
	Data     json.RawMessage `json:"data"`
	Style    json.RawMessage `json:"style"`
	Position *position       `json:"position"`
	Geometry *geometry       `json:"geometry"`
	Parent   *struct {
		ID id `json:"id"`
	} `json:"parent"`
	ModifiedAt string `json:"modifiedAt"`
}

// itemsPage is GET /v2/boards/{id}/items (GenericItemCursorPaged): cursor
// paging, the next page's cursor in the answer.
type itemsPage struct {
	Data   []item `json:"data"`
	Total  int    `json:"total"`
	Cursor string `json:"cursor"`
}

// taggedPage is GET /v2/boards/{id}/items?tag_id= (ItemPagedResponse):
// offset paging.
type taggedPage struct {
	Data   []item `json:"data"`
	Total  int    `json:"total"`
	Offset int    `json:"offset"`
}

// endpoint is a connector's end: the item it is on, and where on it (each a
// percentage of the item's width and height, "50%").
type endpoint struct {
	ID       id `json:"id"`
	Position *struct {
		X num `json:"x"`
		Y num `json:"y"`
	} `json:"position"`
}

// connector is one line between two items (ConnectorWithLinks).
type connector struct {
	ID          id        `json:"id"`
	StartItem   *endpoint `json:"startItem"`
	EndItem     *endpoint `json:"endItem"`
	Shape       string    `json:"shape"`
	IsSupported *bool     `json:"isSupported"`
	Captions    []struct {
		Content  string `json:"content"`
		Position string `json:"position"`
	} `json:"captions"`
	Style *struct {
		Color          string `json:"color"`
		StrokeColor    string `json:"strokeColor"`
		StrokeWidth    num    `json:"strokeWidth"`
		StrokeStyle    string `json:"strokeStyle"`
		StartStrokeCap string `json:"startStrokeCap"`
		EndStrokeCap   string `json:"endStrokeCap"`
		FontSize       num    `json:"fontSize"`
	} `json:"style"`
}

// connectorsPage is GET /v2/boards/{id}/connectors (ConnectorsCursorPaged).
type connectorsPage struct {
	Data   []connector `json:"data"`
	Total  int         `json:"total"`
	Cursor string      `json:"cursor"`
}

// tag is one of a board's tags (TagWithLinks).
type tag struct {
	ID        id     `json:"id"`
	Title     string `json:"title"`
	FillColor string `json:"fillColor"`
}

// tagsPage is GET /v2/boards/{id}/tags (TagsPagedResponse): offset paging.
type tagsPage struct {
	Data   []tag `json:"data"`
	Total  int   `json:"total"`
	Offset int   `json:"offset"`
}

// The data and style each item type carries, read when it is drawn.
type (
	stickyData struct {
		Content string `json:"content"`
		Shape   string `json:"shape"`
	}
	stickyStyle struct {
		FillColor         string `json:"fillColor"`
		TextAlign         string `json:"textAlign"`
		TextAlignVertical string `json:"textAlignVertical"`
	}
	shapeData struct {
		Content string `json:"content"`
		Shape   string `json:"shape"`
	}
	shapeStyle struct {
		BorderColor       string `json:"borderColor"`
		BorderOpacity     *num   `json:"borderOpacity"`
		BorderStyle       string `json:"borderStyle"`
		BorderWidth       *num   `json:"borderWidth"`
		Color             string `json:"color"`
		FillColor         string `json:"fillColor"`
		FillOpacity       *num   `json:"fillOpacity"`
		FontFamily        string `json:"fontFamily"`
		FontSize          num    `json:"fontSize"`
		TextAlign         string `json:"textAlign"`
		TextAlignVertical string `json:"textAlignVertical"`
	}
	textData struct {
		Content string `json:"content"`
	}
	textStyle struct {
		Color       string `json:"color"`
		FillColor   string `json:"fillColor"`
		FillOpacity *num   `json:"fillOpacity"`
		FontFamily  string `json:"fontFamily"`
		FontSize    num    `json:"fontSize"`
		TextAlign   string `json:"textAlign"`
	}
	frameData struct {
		Title  string `json:"title"`
		Format string `json:"format"`
		Type   string `json:"type"`
	}
	frameStyle struct {
		FillColor string `json:"fillColor"`
	}
	imageData struct {
		ImageURL string `json:"imageUrl"`
		Title    string `json:"title"`
	}
	cardData struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		DueDate     string `json:"dueDate"`
		AssigneeID  id     `json:"assigneeId"`
	}
	cardStyle struct {
		CardTheme string `json:"cardTheme"`
	}
	appCardData struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Status      string `json:"status"`
		Fields      []struct {
			Value     string `json:"value"`
			Tooltip   string `json:"tooltip"`
			FillColor string `json:"fillColor"`
		} `json:"fields"`
	}
	documentData struct {
		Title       string `json:"title"`
		DocumentURL string `json:"documentUrl"`
	}
	embedData struct {
		URL          string `json:"url"`
		Title        string `json:"title"`
		ProviderName string `json:"providerName"`
		Description  string `json:"description"`
	}
	previewData struct {
		URL   string `json:"url"`
		Title string `json:"title"`
	}
)
