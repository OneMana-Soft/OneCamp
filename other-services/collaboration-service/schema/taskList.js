// To-do lists (a doc's own block on the web: blocks/doc.ts).
import { TaskList } from '@tiptap/extension-task-list'
import { TaskItem } from '@tiptap/extension-task-item'

export const TaskListSchema = TaskList.configure({
    HTMLAttributes: { class: 'task-list' },
})

export const TaskItemSchema = TaskItem.configure({
    HTMLAttributes: { class: 'task-item' },
    nested: true,
})
