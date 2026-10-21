package models

// A project as read for one person (IsProjectMember and IsProjectAdmin count
// that person), and what Work Access says they may do in it and its tasks
// (business/WorkAccess fills Access: the server's answer, never a guess the web
// makes).

// ReaderIsIn reports whether the person the project was read for is in it,
// as a member or one of its admins: they see its tasks and hear its topic.
func (p *DgraphProject) ReaderIsIn() bool {
	return p != nil && (p.IsProjectMember > 0 || p.IsProjectAdmin > 0)
}

// ProjectAccess is what someone may do in a project, as the app shows it
// (project_access): their role, and each thing the rule lets them do. Never
// stored; set on a project read for them, after the read.
type ProjectAccess struct {
	// Role is admin, member, commenter or viewer; "" outside the project.
	Role    string `json:"role"`
	View    bool   `json:"view"`
	Comment bool   `json:"comment"`
	// CreateTasks makes tasks and sub-tasks.
	CreateTasks bool `json:"create_tasks"`
	// EditTasks is which tasks they change: all, those assigned to them
	// (assigned), or none.
	EditTasks string `json:"edit_tasks"`
	// DeleteTasks is which they delete or restore: all, those they made
	// (own), or none.
	DeleteTasks  string `json:"delete_tasks"`
	LogTime      bool   `json:"log_time"`
	PostUpdates  bool   `json:"post_updates"`
	AddFiles     bool   `json:"add_files"`
	Manage       bool   `json:"manage"`
	ManagePeople bool   `json:"manage_people"`
	Archive      bool   `json:"archive"`
}

// TaskAccess is what someone may do with one task (task_access): the
// project's rule applied to the task. Never stored.
type TaskAccess struct {
	Edit    bool `json:"edit"`
	Delete  bool `json:"delete"`
	Comment bool `json:"comment"`
	LogTime bool `json:"log_time"`
	// Restrict is whether they may choose who sees the task (WA2): its
	// project's admins, and its creator while they may change it.
	Restrict bool `json:"restrict"`
}
