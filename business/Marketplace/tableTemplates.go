package business

// OneCamp's own table templates (gsheets-structure-05): a CRM, hiring, a
// content calendar, stock and a budget, each two or three tables linked to
// each other, with the rollups that read naturally across them, the views each
// needs and a few rows that show them. Their ids are fixed (the web asks for
// them by id). One installs through the one way several tables are made at
// once, ImportBaseAs, as the member, so it can make only what they could make
// by hand; until bases (A5) are built its tables are in the workspace's one
// base. The first table is the one that opens.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	tableDomain "github.com/akashc777/OneCamp/domain/DataTable"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	marketplaceModel "github.com/akashc777/OneCamp/models/postgres/Marketplace"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// BuiltinTableTemplate is one of OneCamp's own table templates.
type BuiltinTableTemplate struct {
	ID                                      uuid.UUID
	Name, Description, Hue, Glyph, Category string
	Tables                                  []TemplateTable
}

// TemplateTable is one table of a template, as an import brings it: its
// fields, views and rows by their keys.
type TemplateTable struct {
	Key, Name, Description, Icon string
	Fields                       []dataTableBusiness.ImportField
	Views                        []dataTableBusiness.ImportView
	Rows                         []dataTableBusiness.ImportRow
}

// BuiltinTableTemplates are OneCamp's table templates, in the order the
// gallery and "New table" show them.
var BuiltinTableTemplates = []BuiltinTableTemplate{
	{
		ID: uuid.MustParse("b5d0c000-0000-4000-8000-000000000101"), Name: "CRM",
		Description: "Deals on a pipeline, the companies they're with and the people you talk to.",
		Hue:         "sky", Glyph: "handshake", Category: "Sales",
		Tables: []TemplateTable{{
			Key: "deals", Name: "Deals", Description: "Every deal, its stage, value and when it should close.",
			Fields: []dataTableBusiness.ImportField{
				text("name", "Deal"),
				choice("stage", "Stage", "Lead", "Qualified", "Proposal", "Won", "Lost"),
				money("value", "Value"),
				typed("close", "Close date", model.FieldDate),
				link("company", "Company", "companies", "deals"),
				formulaOf("open", "Open value", `IF({Stage} = "Won", 0, IF({Stage} = "Lost", 0, {Value}))`, currency),
			},
			Views: []dataTableBusiness.ImportView{board("Pipeline", "stage"), view("All deals", tableDomain.ViewGrid)},
			Rows: []dataTableBusiness.ImportRow{
				row("d1", cells{"name": "Website redesign", "stage": "Proposal", "value": 12000.0, "close": relDay(10)}, links{"company": {"c1"}}),
				row("d2", cells{"name": "Annual support", "stage": "Won", "value": 8000.0, "close": relDay(-5)}, links{"company": {"c1"}}),
				row("d3", cells{"name": "Pilot", "stage": "Qualified", "value": 5000.0, "close": relDay(20)}, links{"company": {"c2"}}),
				row("d4", cells{"name": "Data migration", "stage": "Lead", "value": 15000.0, "close": relDay(30)}, links{"company": {"c3"}}),
				row("d5", cells{"name": "Training", "stage": "Lost", "value": 3000.0, "close": relDay(-10)}, links{"company": {"c2"}}),
			},
		}, {
			Key: "companies", Name: "Companies", Description: "Who you sell to, and what's open with each.",
			Fields: []dataTableBusiness.ImportField{
				text("name", "Company"),
				choice("industry", "Industry", "Software", "Retail", "Health", "Finance"),
				typed("site", "Website", model.FieldURL),
				link("deals", "Deals", "deals", "company"),
				link("contacts", "Contacts", "contacts", "company"),
				rollupOf("openValue", "Open deal value", "deals", "open", "sum", currency),
			},
			Views: []dataTableBusiness.ImportView{view("All companies", tableDomain.ViewGrid)},
			Rows: []dataTableBusiness.ImportRow{
				row("c1", cells{"name": "Northwind", "industry": "Retail", "site": "https://northwind.example.com"}, nil),
				row("c2", cells{"name": "Lakeside Health", "industry": "Health", "site": "https://lakeside.example.org"}, nil),
				row("c3", cells{"name": "Bluebird Finance", "industry": "Finance", "site": "https://bluebird.example.com"}, nil),
			},
		}, {
			Key: "contacts", Name: "Contacts", Description: "The people at each company.",
			Fields: []dataTableBusiness.ImportField{
				text("name", "Name"),
				typed("email", "Email", model.FieldEmail),
				text("role", "Role"),
				link("company", "Company", "companies", "contacts"),
			},
			Views: []dataTableBusiness.ImportView{view("Everyone", tableDomain.ViewGrid), view("Cards", tableDomain.ViewGallery)},
			Rows: []dataTableBusiness.ImportRow{
				row("p1", cells{"name": "Maya Patel", "email": "maya@example.com", "role": "Head of operations"}, links{"company": {"c1"}}),
				row("p2", cells{"name": "Tom Okafor", "email": "tom@example.org", "role": "CTO"}, links{"company": {"c2"}}),
				row("p3", cells{"name": "Lena Fischer", "email": "lena@example.com", "role": "Finance lead"}, links{"company": {"c3"}}),
			},
		}},
	},
	{
		ID: uuid.MustParse("b5d0c000-0000-4000-8000-000000000102"), Name: "Hiring",
		Description: "Candidates on a pipeline, the roles they're for and their interviews.",
		Hue:         "berry", Glyph: "user-plus", Category: "People",
		Tables: []TemplateTable{{
			Key: "candidates", Name: "Candidates", Description: "Everyone who applied, and how far they've come.",
			Fields: []dataTableBusiness.ImportField{
				text("name", "Name"),
				typed("email", "Email", model.FieldEmail),
				choice("stage", "Stage", "Applied", "Screen", "Interview", "Offer", "Hired", "Rejected"),
				typed("applied", "Applied on", model.FieldDate),
				link("role", "Role", "roles", "candidates"),
				link("interviews", "Interviews", "interviews", "candidate"),
				rollupOf("score", "Score", "interviews", "score", "average", nil),
				formulaOf("hired", "Hired", `IF({Stage} = "Hired", 1, 0)`, nil),
			},
			Views: []dataTableBusiness.ImportView{board("Pipeline", "stage"), view("All candidates", tableDomain.ViewGrid)},
			Rows: []dataTableBusiness.ImportRow{
				row("k1", cells{"name": "Ana Silva", "email": "ana@example.com", "stage": "Interview", "applied": relDay(-6)}, links{"role": {"r1"}}),
				row("k2", cells{"name": "Ben Carter", "email": "ben@example.org", "stage": "Offer", "applied": relDay(-12)}, links{"role": {"r1"}}),
				row("k3", cells{"name": "Chloe Kim", "email": "chloe@example.com", "stage": "Hired", "applied": relDay(-30)}, links{"role": {"r2"}}),
				row("k4", cells{"name": "Dev Shah", "email": "dev@example.com", "stage": "Applied", "applied": relDay(-1)}, links{"role": {"r2"}}),
			},
		}, {
			Key: "roles", Name: "Roles", Description: "The roles you're hiring for, and how many are still open.",
			Fields: []dataTableBusiness.ImportField{
				text("name", "Role"),
				choice("team", "Team", "Engineering", "Design", "Sales"),
				typed("openings", "Openings", model.FieldNumber),
				link("candidates", "Candidates", "candidates", "role"),
				rollupOf("applicants", "Applicants", "candidates", "name", "count", nil),
				rollupOf("hired", "Hired", "candidates", "hired", "sum", nil),
				formulaOf("left", "Still to hire", `{Openings} - {Hired}`, nil),
			},
			Views: []dataTableBusiness.ImportView{view("Open roles", tableDomain.ViewGrid)},
			Rows: []dataTableBusiness.ImportRow{
				row("r1", cells{"name": "Backend engineer", "team": "Engineering", "openings": 2.0}, nil),
				row("r2", cells{"name": "Product designer", "team": "Design", "openings": 1.0}, nil),
				row("r3", cells{"name": "Account executive", "team": "Sales", "openings": 1.0}, nil),
			},
		}, {
			Key: "interviews", Name: "Interviews", Description: "Who meets whom, when, and how it went.",
			Fields: []dataTableBusiness.ImportField{
				text("name", "Interview"),
				typed("date", "Date", model.FieldDate),
				{Key: "score", Name: "Score", Type: model.FieldRating, Config: map[string]any{"max": 5.0}},
				typed("notes", "Notes", model.FieldLongText),
				link("candidate", "Candidate", "candidates", "interviews"),
			},
			Views: []dataTableBusiness.ImportView{view("Schedule", tableDomain.ViewCalendar), view("All interviews", tableDomain.ViewGrid)},
			Rows: []dataTableBusiness.ImportRow{
				row("i1", cells{"name": "Ana: technical", "date": relDay(2)}, links{"candidate": {"k1"}}),
				row("i2", cells{"name": "Ben: final", "date": relDay(-3), "score": 4.0, "notes": "Clear on trade-offs."}, links{"candidate": {"k2"}}),
				row("i3", cells{"name": "Ben: technical", "date": relDay(-8), "score": 5.0}, links{"candidate": {"k2"}}),
				row("i4", cells{"name": "Chloe: portfolio", "date": relDay(-25), "score": 5.0}, links{"candidate": {"k3"}}),
			},
		}},
	},
	{
		ID: uuid.MustParse("b5d0c000-0000-4000-8000-000000000103"), Name: "Content calendar",
		Description: "Posts on a calendar and a pipeline, grouped into campaigns.",
		Hue:         "sun", Glyph: "calendar", Category: "Marketing",
		Tables: []TemplateTable{{
			Key: "posts", Name: "Posts", Description: "Each piece, where it goes out and when.",
			Fields: []dataTableBusiness.ImportField{
				text("name", "Title"),
				choice("status", "Status", "Idea", "Drafting", "Review", "Scheduled", "Published"),
				typed("date", "Publish on", model.FieldDate),
				choice("channel", "Channel", "Blog", "Newsletter", "LinkedIn", "Video"),
				link("campaign", "Campaign", "campaigns", "posts"),
				formulaOf("published", "Published", `IF({Status} = "Published", 1, 0)`, nil),
			},
			Views: []dataTableBusiness.ImportView{view("Calendar", tableDomain.ViewCalendar), board("Pipeline", "status"), view("All posts", tableDomain.ViewGrid)},
			Rows: []dataTableBusiness.ImportRow{
				row("s1", cells{"name": "Launch announcement", "status": "Published", "date": relDay(-3), "channel": "Blog"}, links{"campaign": {"m1"}}),
				row("s2", cells{"name": "Customer story: Northwind", "status": "Scheduled", "date": relDay(4), "channel": "Blog"}, links{"campaign": {"m1"}}),
				row("s3", cells{"name": "Five tips for remote teams", "status": "Drafting", "date": relDay(9), "channel": "LinkedIn"}, links{"campaign": {"m2"}}),
				row("s4", cells{"name": "Monthly digest", "status": "Review", "date": relDay(14), "channel": "Newsletter"}, links{"campaign": {"m2"}}),
				row("s5", cells{"name": "Product walkthrough", "status": "Idea", "date": relDay(18), "channel": "Video"}, links{"campaign": {"m1"}}),
			},
		}, {
			Key: "campaigns", Name: "Campaigns", Description: "What the posts add up to, and how many are out.",
			Fields: []dataTableBusiness.ImportField{
				text("name", "Campaign"),
				typed("goal", "Goal", model.FieldLongText),
				typed("starts", "Starts", model.FieldDate),
				typed("ends", "Ends", model.FieldDate),
				link("posts", "Posts", "posts", "campaign"),
				rollupOf("count", "Pieces", "posts", "name", "count", nil),
				rollupOf("out", "Out", "posts", "published", "sum", nil),
			},
			Views: []dataTableBusiness.ImportView{view("Campaigns", tableDomain.ViewGrid)},
			Rows: []dataTableBusiness.ImportRow{
				row("m1", cells{"name": "Autumn launch", "goal": "Tell everyone what's new.", "starts": relDay(-7), "ends": relDay(21)}, nil),
				row("m2", cells{"name": "Remote work series", "goal": "Help teams that work apart.", "starts": relDay(-1), "ends": relDay(30)}, nil),
				row("m3", cells{"name": "Year in review", "goal": "Look back on the year.", "starts": relDay(30), "ends": relDay(45)}, nil),
			},
		}},
	},
	{
		ID: uuid.MustParse("b5d0c000-0000-4000-8000-000000000104"), Name: "Stock",
		Description: "Products and what's on hand, every delivery and sale, and who supplies them.",
		Hue:         "moss", Glyph: "package", Category: "Operations",
		Tables: []TemplateTable{{
			Key: "products", Name: "Products", Description: "What you keep, how many are on hand, and when to order more.",
			Fields: []dataTableBusiness.ImportField{
				text("name", "Product"),
				text("sku", "SKU"),
				choice("category", "Category", "Hardware", "Accessories", "Cables"),
				typed("reorder", "Reorder at", model.FieldNumber),
				link("moves", "Movements", "movements", "product"),
				link("supplier", "Supplier", "suppliers", "products"),
				rollupOf("stock", "In stock", "moves", "change", "sum", nil),
				formulaOf("status", "Status", `IF({In stock} <= {Reorder at}, "Reorder", "OK")`, nil),
			},
			Views: []dataTableBusiness.ImportView{view("Stock", tableDomain.ViewGrid), board("By category", "category")},
			Rows: []dataTableBusiness.ImportRow{
				row("p1", cells{"name": "USB-C hub", "sku": "HUB-01", "category": "Accessories", "reorder": 10.0}, links{"supplier": {"s1"}}),
				row("p2", cells{"name": "27-inch monitor", "sku": "MON-27", "category": "Hardware", "reorder": 3.0}, links{"supplier": {"s2"}}),
				row("p3", cells{"name": "HDMI cable, 2 m", "sku": "CAB-HD2", "category": "Cables", "reorder": 20.0}, links{"supplier": {"s1"}}),
			},
		}, {
			Key: "movements", Name: "Movements", Description: "Every delivery in and sale out, as a change in stock.",
			Fields: []dataTableBusiness.ImportField{
				text("name", "Movement"),
				typed("date", "Date", model.FieldDate),
				choice("kind", "Kind", "Received", "Sold", "Adjusted"),
				typed("change", "Change", model.FieldNumber),
				link("product", "Product", "products", "moves"),
			},
			Views: []dataTableBusiness.ImportView{view("Log", tableDomain.ViewGrid), view("By day", tableDomain.ViewCalendar)},
			Rows: []dataTableBusiness.ImportRow{
				row("m1", cells{"name": "First delivery", "date": relDay(-20), "kind": "Received", "change": 40.0}, links{"product": {"p1"}}),
				row("m2", cells{"name": "Webshop orders", "date": relDay(-6), "kind": "Sold", "change": -32.0}, links{"product": {"p1"}}),
				row("m3", cells{"name": "Monitor delivery", "date": relDay(-15), "kind": "Received", "change": 8.0}, links{"product": {"p2"}}),
				row("m4", cells{"name": "Office order", "date": relDay(-2), "kind": "Sold", "change": -2.0}, links{"product": {"p2"}}),
				row("m5", cells{"name": "Cable delivery", "date": relDay(-10), "kind": "Received", "change": 50.0}, links{"product": {"p3"}}),
				row("m6", cells{"name": "Count correction", "date": relDay(-1), "kind": "Adjusted", "change": -1.0}, links{"product": {"p3"}}),
			},
		}, {
			Key: "suppliers", Name: "Suppliers", Description: "Who you order from, and how long they take.",
			Fields: []dataTableBusiness.ImportField{
				text("name", "Supplier"),
				typed("email", "Email", model.FieldEmail),
				typed("lead", "Lead time (days)", model.FieldNumber),
				link("products", "Products", "products", "supplier"),
			},
			Views: []dataTableBusiness.ImportView{view("Suppliers", tableDomain.ViewGrid)},
			Rows: []dataTableBusiness.ImportRow{
				row("s1", cells{"name": "Cable & Co", "email": "orders@example.com", "lead": 5.0}, nil),
				row("s2", cells{"name": "Brightview Displays", "email": "sales@example.org", "lead": 14.0}, nil),
				row("s3", cells{"name": "Deskline", "email": "hello@example.com", "lead": 7.0}, nil),
			},
		}},
	},
	{
		ID: uuid.MustParse("b5d0c000-0000-4000-8000-000000000105"), Name: "Budget",
		Description: "Categories with a budget, every expense in them, and what's left.",
		Hue:         "lake", Glyph: "wallet", Category: "Finance",
		Tables: []TemplateTable{{
			Key: "categories", Name: "Categories", Description: "What you plan to spend on each, what's spent and what's left.",
			Fields: []dataTableBusiness.ImportField{
				text("name", "Category"),
				money("budget", "Budget"),
				link("expenses", "Expenses", "expenses", "category"),
				rollupOf("spent", "Spent", "expenses", "amount", "sum", currency),
				formulaOf("left", "Left", `{Budget} - {Spent}`, currency),
				formulaOf("status", "Status", `IF({Spent} > {Budget}, "Over budget", "Within budget")`, nil),
			},
			Views: []dataTableBusiness.ImportView{view("Budget", tableDomain.ViewGrid)},
			Rows: []dataTableBusiness.ImportRow{
				row("c1", cells{"name": "Software", "budget": 1200.0}, nil),
				row("c2", cells{"name": "Travel", "budget": 3000.0}, nil),
				row("c3", cells{"name": "Events", "budget": 2500.0}, nil),
				row("c4", cells{"name": "Office", "budget": 800.0}, nil),
			},
		}, {
			Key: "expenses", Name: "Expenses", Description: "Every expense, what it was for and how it was paid.",
			Fields: []dataTableBusiness.ImportField{
				text("name", "Expense"),
				money("amount", "Amount"),
				typed("date", "Date", model.FieldDate),
				choice("paid", "Paid with", "Card", "Transfer", "Cash"),
				link("category", "Category", "categories", "expenses"),
			},
			Views: []dataTableBusiness.ImportView{view("All expenses", tableDomain.ViewGrid), view("By date", tableDomain.ViewCalendar), board("By payment", "paid")},
			Rows: []dataTableBusiness.ImportRow{
				row("e1", cells{"name": "Design tool seats", "amount": 480.0, "date": relDay(-12), "paid": "Card"}, links{"category": {"c1"}}),
				row("e2", cells{"name": "Cloud hosting", "amount": 900.0, "date": relDay(-4), "paid": "Card"}, links{"category": {"c1"}}),
				row("e3", cells{"name": "Flights to Berlin", "amount": 1150.0, "date": relDay(-20), "paid": "Card"}, links{"category": {"c2"}}),
				row("e4", cells{"name": "Hotel", "amount": 640.0, "date": relDay(-18), "paid": "Transfer"}, links{"category": {"c2"}}),
				row("e5", cells{"name": "Meetup venue", "amount": 700.0, "date": relDay(-9), "paid": "Transfer"}, links{"category": {"c3"}}),
				row("e6", cells{"name": "Coffee and snacks", "amount": 60.0, "date": relDay(-2), "paid": "Cash"}, links{"category": {"c4"}}),
			},
		}},
	},
}

// relDay is a date cell as so many days from the day the template is
// installed, so its calendar opens on rows of this month, not of the day it
// was written.
type relDay int

type cells = map[string]any
type links = map[string][]string

// currency shows a number as money.
var currency = map[string]any{"format": map[string]any{"currency": "USD", "decimals": 0.0}}

func text(key, name string) dataTableBusiness.ImportField { return typed(key, name, model.FieldText) }

func typed(key, name, kind string) dataTableBusiness.ImportField {
	return dataTableBusiness.ImportField{Key: key, Name: name, Type: kind}
}

func money(key, name string) dataTableBusiness.ImportField {
	f := typed(key, name, model.FieldNumber)
	f.Config = currency
	return f
}

// choice is a select of labels, each with an id of its own.
func choice(key, name string, labels ...string) dataTableBusiness.ImportField {
	opts := make([]any, len(labels))
	for i, l := range labels {
		opts[i] = map[string]any{"id": fmt.Sprintf("%s-%d", key, i+1), "label": l}
	}
	f := typed(key, name, model.FieldSelect)
	f.Config = map[string]any{"options": opts}
	return f
}

func link(key, name, to, pair string) dataTableBusiness.ImportField {
	f := typed(key, name, model.FieldRelation)
	f.LinksTo, f.Pair = to, pair
	return f
}

func rollupOf(key, name, through, of, how string, cfg map[string]any) dataTableBusiness.ImportField {
	f := typed(key, name, model.FieldRollup)
	f.Through, f.Of, f.Aggregate, f.Config = through, of, how, cfg
	return f
}

func formulaOf(key, name, formula string, cfg map[string]any) dataTableBusiness.ImportField {
	f := typed(key, name, model.FieldFormula)
	f.Formula, f.Config = formula, cfg
	return f
}

func view(name, kind string) dataTableBusiness.ImportView {
	return dataTableBusiness.ImportView{Name: name, Type: kind}
}

// board is a board whose columns are the select groupBy names.
func board(name, groupBy string) dataTableBusiness.ImportView {
	return dataTableBusiness.ImportView{Name: name, Type: tableDomain.ViewBoard, GroupBy: groupBy}
}

func row(key string, c cells, l links) dataTableBusiness.ImportRow {
	return dataTableBusiness.ImportRow{Key: key, Cells: c, Links: l}
}

// base is the template as an import brings it, its dates from today. Each
// install gets its own copy: an import names its fields as it makes them,
// and a field's config may be added to.
func (b BuiltinTableTemplate) base(today time.Time) dataTableBusiness.ImportBase {
	var out dataTableBusiness.ImportBase
	for _, tb := range b.Tables {
		rows := make([]dataTableBusiness.ImportRow, len(tb.Rows))
		for i, r := range tb.Rows {
			c := make(cells, len(r.Cells))
			for k, v := range r.Cells {
				if d, ok := v.(relDay); ok {
					v = today.AddDate(0, 0, int(d)).Format("2006-01-02")
				}
				c[k] = v
			}
			rows[i] = row(r.Key, c, r.Links)
		}
		fields := make([]dataTableBusiness.ImportField, len(tb.Fields))
		for i, f := range tb.Fields {
			f.Config = copyConfig(f.Config)
			fields[i] = f
		}
		out.Tables = append(out.Tables, dataTableBusiness.ImportTable{Key: tb.Key, Name: tb.Name, Description: tb.Description, Icon: tb.Icon,
			Fields: fields, Views: append([]dataTableBusiness.ImportView(nil), tb.Views...),
			Rows: func(page func([]dataTableBusiness.ImportRow) error) error { return page(rows) }})
	}
	return out
}

func copyConfig(cfg map[string]any) map[string]any {
	if cfg == nil {
		return nil
	}
	var out map[string]any
	b, _ := json.Marshal(cfg)
	_ = json.Unmarshal(b, &out)
	return out
}

// builtinTableByID is one of OneCamp's table templates by id.
func builtinTableByID(id uuid.UUID) (BuiltinTableTemplate, bool) {
	for _, b := range BuiltinTableTemplates {
		if b.ID == id {
			return b, true
		}
	}
	return BuiltinTableTemplate{}, false
}

// tableTemplatePayload is what a table template of OneCamp's says of itself
// in a list: the tables it makes.
type tableTemplatePayload struct {
	Builtin  bool                `json:"builtin"`
	Hue      string              `json:"hue"`
	Glyph    string              `json:"glyph"`
	Category string              `json:"category"`
	Tables   []tableTemplateLine `json:"tables"`
}

type tableTemplateLine struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// builtinTableTemplate is one of OneCamp's table templates as the
// marketplace answers it.
func builtinTableTemplate(b BuiltinTableTemplate) *marketplaceModel.Template {
	p := tableTemplatePayload{Builtin: true, Hue: b.Hue, Glyph: b.Glyph, Category: b.Category}
	for _, tb := range b.Tables {
		p.Tables = append(p.Tables, tableTemplateLine{Name: tb.Name, Description: tb.Description})
	}
	payload, _ := json.Marshal(p)
	desc := b.Description
	return &marketplaceModel.Template{Id: b.ID, Kind: marketplaceModel.KindTable, Name: b.Name, Description: &desc,
		Payload: string(payload), CreatedBy: uuid.Nil, CreatedAt: builtinEpoch, UpdatedAt: builtinEpoch, AuthorName: "OneCamp"}
}

// tableBuiltinsListed is OneCamp's table templates, in their order.
func tableBuiltinsListed() []*marketplaceModel.Template {
	out := make([]*marketplaceModel.Template, 0, len(BuiltinTableTemplates))
	for _, b := range BuiltinTableTemplates {
		out = append(out, builtinTableTemplate(b))
	}
	return out
}

// installBuiltinTable makes a table template's tables for the member, as
// they'd make them by hand, through ImportBaseAs: an install that fails part
// way takes back what it made. It opens on the first table; its tables are
// answered in the template's order.
func installBuiltinTable(ctx context.Context, b BuiltinTableTemplate, userInfo *userModels.UserInfo) (*InstallResult, []madeTable, error) {
	actor := dataTableBusiness.Actor{UserID: userInfo.UserPostgresInfo.Id, IsAdmin: userInfo.UserPostgresInfo.IsAdmin}
	made, err := dataTableBusiness.ImportBaseAs(ctx, actor, b.base(time.Now()))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to install %s: %w", b.Name, err)
	}
	for _, n := range made.Notes {
		helpers.LogErrorWithContext(ctx, "marketplace: the %s template left out %s/%s: %s", b.Name, n.Table, n.Field, n.Msg)
	}
	tables := make([]madeTable, 0, len(b.Tables))
	for _, t := range b.Tables {
		tables = append(tables, madeTable{ID: made.Tables[t.Key], Name: t.Name})
	}
	return &InstallResult{Kind: marketplaceModel.KindTable, EntityID: made.Tables[b.Tables[0].Key].String(), Name: b.Name}, tables, nil
}
