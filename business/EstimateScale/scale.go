// Package business is a project's estimate scale (linear-05). A project
// estimates in hours (task_estimate_minutes) unless it has a scale: points on
// Linear's exponential, Fibonacci, linear or T-shirt scale, which burndown,
// velocity and capacity then count. Hours and logged time stay beside points,
// so a task says both how big it is and how long it took, which Linear, with
// no time, can't.
package business

import (
	"context"
	"errors"
	"slices"
	"strconv"

	models "github.com/akashc777/OneCamp/models/postgres/EstimateScale"
	"github.com/google/uuid"
)

// Scale is a project's scale; nil is hours.
type Scale = models.Scale

// Kind is one of the scales a project may count in.
type Kind struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Sizes are its points, smallest first, and More the two larger ones the
	// extended scale adds.
	Sizes []float64 `json:"sizes"`
	More  []float64 `json:"more"`
	// Names name the T-shirt sizes, Sizes' then More's; the others are their
	// numbers. A T-shirt size is kept as its number, as Linear keeps it.
	Names []string `json:"names,omitempty"`
}

// Kinds are Linear's four scales, in the order Linear lists them.
var Kinds = []Kind{
	{Kind: "exponential", Name: "Exponential", Sizes: []float64{1, 2, 4, 8, 16}, More: []float64{32, 64}},
	{Kind: "fibonacci", Name: "Fibonacci", Sizes: []float64{1, 2, 3, 5, 8}, More: []float64{13, 21}},
	{Kind: "linear", Name: "Linear", Sizes: []float64{1, 2, 3, 4, 5}, More: []float64{6, 7}},
	{Kind: "tshirt", Name: "T-shirt", Sizes: []float64{1, 2, 3, 5, 8}, More: []float64{13, 21}, Names: []string{"XS", "S", "M", "L", "XL", "XXL", "XXXL"}},
}

// Hours is the kind a project with no scale counts in.
const Hours = "hours"

var (
	ErrUnknownKind = errors.New("a scale is hours, exponential, fibonacci, linear or tshirt")
	// ErrHours: a task's points, in a project that estimates in hours.
	ErrHours = errors.New("this project estimates in hours")
	// ErrNotASize: points that aren't one of the scale's sizes.
	ErrNotASize = errors.New("those points aren't a size on this project's scale")
)

// KindOf is the scale named kind.
func KindOf(kind string) (Kind, bool) {
	i := slices.IndexFunc(Kinds, func(k Kind) bool { return k.Kind == kind })
	if i < 0 {
		return Kind{}, false
	}
	return Kinds[i], true
}

// Sizes are the points a task may take on s, smallest first: zero when it is
// a size, then the scale's sizes, then the two larger ones when extended.
func Sizes(s *Scale) []float64 {
	k, ok := KindOf(s.Kind)
	if !ok {
		return nil
	}
	var out []float64
	if s.AllowZero {
		out = append(out, 0)
	}
	out = append(out, k.Sizes...)
	if s.Extended {
		out = append(out, k.More...)
	}
	return out
}

// Label names points on s: a T-shirt size by its name ("M"), any other by its
// number ("3"). Points off the scale (kept from an import, or from before the
// scale changed) are their number.
func Label(s *Scale, points float64) string {
	if k, ok := KindOf(s.Kind); ok && len(k.Names) > 0 {
		if i := slices.Index(append(slices.Clone(k.Sizes), k.More...), points); i >= 0 {
			return k.Names[i]
		}
	}
	return strconv.FormatFloat(points, 'f', -1, 64)
}

// Allows says whether points are a size on s.
func Allows(s *Scale, points float64) bool {
	return slices.Contains(Sizes(s), points)
}

// Counted is what a task counts as on s: its points, or, with none, one when
// the scale counts unestimated tasks and nothing when it doesn't.
func Counted(s *Scale, points *float64) float64 {
	switch {
	case points != nil:
		return *points
	case s.CountUnestimated:
		return 1
	}
	return 0
}

// CountedAll is what tasks tasks count as on s, pointed of them with points
// that add up to points: Counted for each, from a graph's counts and sums
// rather than a task at a time.
func CountedAll(s *Scale, tasks, pointed int, points float64) float64 {
	return points + float64(tasks-pointed)*Counted(s, nil)
}

// View is a scale as its project's people see it, with its sizes and their
// names in order. Nil is hours.
type View struct {
	*Scale
	Sizes  []float64 `json:"sizes"`
	Labels []string  `json:"labels"`
}

// ViewOf is s's view, or nil for hours.
func ViewOf(s *Scale) *View {
	if s == nil {
		return nil
	}
	v := &View{Scale: s, Sizes: Sizes(s)}
	for _, p := range v.Sizes {
		v.Labels = append(v.Labels, Label(s, p))
	}
	return v
}

// Of is a project's scale, or nil when it estimates in hours.
func Of(ctx context.Context, project uuid.UUID) (*Scale, error) {
	return models.Of(ctx, project)
}

// OfMany is the scales of projects, by project, in one read: none for a
// project that estimates in hours.
func OfMany(ctx context.Context, projects []uuid.UUID) (map[uuid.UUID]*Scale, error) {
	return models.OfMany(ctx, projects)
}

// Set puts a project on a scale, or back on hours (kind "hours"), where its
// tasks' points stay unread until it counts in points again. It answers the
// scale, nil for hours.
func Set(ctx context.Context, project uuid.UUID, s Scale, by uuid.UUID) (*Scale, error) {
	if s.Kind == Hours {
		return nil, models.Clear(ctx, project)
	}
	if _, ok := KindOf(s.Kind); !ok {
		return nil, ErrUnknownKind
	}
	s.ProjectUUID, s.SetBy = project, &by
	return models.Set(ctx, s)
}
