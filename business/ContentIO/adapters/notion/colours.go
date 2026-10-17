package notion

import "strings"

// Notion's colours ("blue", "blue_background", "default") as a doc's: the
// doc colours nearest them (business/ContentCodec colours.go) for text,
// highlights and blocks, and a callout's own colours for a callout. One table
// for everything that reads Notion: its API (business/Import providers/notion)
// and its export (export.go).

var docColours = map[string]string{
	"gray": "gray", "brown": "sun", "orange": "sun", "yellow": "sun", "green": "moss",
	"blue": "sky", "purple": "dusk", "pink": "berry", "red": "berry",
}

var calloutColours = map[string]string{
	"gray": "gray", "brown": "yellow", "orange": "yellow", "yellow": "yellow", "green": "green",
	"blue": "blue", "purple": "purple", "pink": "red", "red": "red",
}

// DocColour is a Notion colour as a doc colour, and whether it is a
// background; "" for the default.
func DocColour(c string) (name string, background bool) {
	base, background := strings.CutSuffix(c, "_background")
	return docColours[base], background
}

// BlockColour is a Notion colour as a block's stored colour ("sky",
// "sky_background"), or "".
func BlockColour(c string) string {
	name, background := DocColour(c)
	if name != "" && background {
		return name + "_background"
	}
	return name
}

// CalloutColour is a Notion colour as a callout's, gray for the default,
// which Notion draws grey.
func CalloutColour(c string) string {
	base, _ := strings.CutSuffix(c, "_background")
	if colour := calloutColours[base]; colour != "" {
		return colour
	}
	return "gray"
}
