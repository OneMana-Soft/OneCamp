//go:build integration

package Domain

// TaskCopiesAmongQueryForTest is the text TaskCopiesAmong runs, so a test can
// ask the planner how it reads tasks. It exists only in the integration
// build, so no product code can reach it.
const TaskCopiesAmongQueryForTest = taskCopiesAmong
