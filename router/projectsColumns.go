package router

import (
	taskStatusController "github.com/akashc777/OneCamp/controllers/TaskStatus"
)

// mountBoardColumns is the board's column order (design/projects-vs-asana-jira/
// BOARD_COLUMNS_PLAN.md): a project's admins drag its columns, and the order
// is the project's, beside today's /project/{p}/statuses routes.
func mountBoardColumns(rt projectsRouters) {
	rt.Project.Post("/{project_uuid}/statuses/order", taskStatusController.Order)
}
