package business

import "errors"

// ErrNotAllowed is something the person may see but not do: every Refusal is
// one (errors.Is). Someone outside a project isn't refused, they find nothing:
// the caller answers them as it answers a project that isn't there.
var ErrNotAllowed = errors.New("work: that isn't yours to do here")

// Refusal says why someone may not do a need, in their words.
type Refusal struct {
	Need     Need
	Standing Standing
}

func (r *Refusal) Error() string { return r.Words() }

// Is makes every Refusal ErrNotAllowed.
func (r *Refusal) Is(target error) bool { return target == ErrNotAllowed }

// Check is nil when standing s may do n on the project, else a Refusal.
func (s Standing) Check(n Need) error {
	if s.Can(n) {
		return nil
	}
	return &Refusal{Need: n, Standing: s}
}

// CheckTask is nil when standing s may do n on a task with facts f, else a
// Refusal.
func (s Standing) CheckTask(n Need, f TaskFacts) error {
	if s.CanOnTask(n, f) {
		return nil
	}
	return &Refusal{Need: n, Standing: s}
}

// Words is what someone refused is told: who can do it, or what they can do
// instead. Pure.
func (r *Refusal) Words() string {
	s := r.Standing
	reader := s.Role == RoleViewer || s.Role == RoleCommenter
	switch r.Need {
	case NeedView:
		return "Project not found"
	case NeedComment:
		return "You can see this project but not comment in it."
	case NeedCreateTask:
		if reader {
			return "You can see this project but not add tasks to it."
		}
		return "Only this project's admins add tasks to it."
	case NeedEditTask:
		switch {
		case reader:
			return "You can see this task but not change it."
		case s.Role == RoleMember && s.EditWho == EditAssigned:
			return "In this project you can change only the tasks assigned to you."
		}
		return "Only this project's admins change its tasks."
	case NeedDeleteTask:
		switch {
		case reader:
			return "You can see this task but not delete it."
		case s.Role == RoleMember && s.membersWork():
			return "You can delete only the tasks you made. Ask one of the project's admins to delete this one."
		}
		return "Only this project's admins delete its tasks."
	case NeedLogTime:
		return "Only the people who work in this project log time on its tasks."
	case NeedPostUpdate:
		return "Only the people who work in this project post its updates."
	case NeedAddFiles:
		return "Only the people who work in this project add files to it."
	case NeedManagePeople:
		return "Only the project's admins can change who is in it."
	case NeedArchive:
		return "Only the project's admins and its team's admins can archive it."
	}
	return "Only the project's admins can change its settings."
}
