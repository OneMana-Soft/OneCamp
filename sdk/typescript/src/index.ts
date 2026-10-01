// OneCamp TypeScript SDK
//
// A small, dependency-free client for the OneCamp public API (/v1). It
// authenticates with a scoped API token (created in OneCamp under Settings >
// API tokens) and runs as that token's owner, narrowed to the token's scopes.
//
// Usage:
//   import { OneCampClient } from "@onecamp/sdk"
//   const oc = new OneCampClient({ token: "oc_...", baseUrl: "https://your-onecamp.example.com" })
//   const me = await oc.me()
//   const tasks = await oc.tasks.list({ status: "inProgress" })
//   await oc.messages.sendToChannel(channelId, "Deploy finished ✅")

export interface OneCampClientOptions {
  /** A scoped API token, e.g. "oc_...". */
  token: string
  /** Base URL of the OneCamp deployment, e.g. "https://onecamp.example.com". */
  baseUrl: string
  /** Optional custom fetch (for Node < 18 or testing). Defaults to global fetch. */
  fetch?: typeof fetch
}

/** Granted scopes a token may carry. */
export type Scope =
  | "tasks:read"
  | "tasks:write"
  | "projects:read"
  | "projects:write"
  | "docs:read"
  | "docs:write"
  | "messages:read"
  | "messages:write"
  | "calendar:write"
  | "tables:read"
  | "tables:write"

/** Error thrown for any non-2xx API response. */
export class OneCampError extends Error {
  readonly status: number
  readonly body: unknown
  constructor(status: number, message: string, body: unknown) {
    super(message)
    this.name = "OneCampError"
    this.status = status
    this.body = body
  }
}

export interface Identity {
  id: string
  is_admin: boolean
  scopes: Scope[]
}

export interface DataTable {
  id: string
  name: string
  description?: string | null
  icon?: string | null
  visibility: "private" | "workspace"
  created_by: string
  created_at: string
  updated_at: string
}

export interface TableField {
  id: string
  table_id: string
  name: string
  type: string
  config: string
  position: number
}

export interface TableRow {
  id: string
  table_id: string
  values: string
  position: number
  created_by?: string | null
  created_at: string
  updated_at: string
}

export interface TableBundle {
  table: DataTable
  fields: TableField[]
  views: unknown[]
  rows: TableRow[]
  can_manage: boolean
  mqtt_topic: string
}

export type AggregateOp = "count" | "sum" | "avg" | "min" | "max"
export type AggregateFilterOp =
  | "eq" | "ne" | "contains" | "gt" | "gte" | "lt" | "lte" | "empty" | "not_empty"

/** A single row-level predicate. `field` is a column id or name. */
export interface AggregateFilter {
  field: string
  op: AggregateFilterOp
  value?: string
}

/** Describes a grouped aggregation over a table's rows. */
export interface AggregateQuery {
  group_by?: string
  aggregate?: AggregateOp
  value_field?: string
  filters?: AggregateFilter[]
  limit?: number
  ascending?: boolean
}

export interface AggregateBucket {
  label: string
  value: number
  count: number
}

export interface AggregateResult {
  aggregate: AggregateOp
  group_by: string
  group_by_type?: string
  value_field?: string
  buckets: AggregateBucket[]
  matched_rows: number
  scanned_rows: number
  distinct_groups: number
  truncated: boolean
}

/** Most write tools return a human-readable message plus optional metadata. */
export interface ToolResult {
  message: string
  data?: Record<string, string> | null
}

export interface CreateTaskInput {
  task_name: string
  project_uuid: string
  description?: string
  priority?: "low" | "medium" | "high" | "urgent"
  assignee_uuid?: string
}

export type TaskStatus =
  | "todo"
  | "inProgress"
  | "inReview"
  | "done"
  | "backlog"
  | "canceled"

export interface ListTasksOptions {
  status?: TaskStatus
  /** "overdue" to show only overdue, not-done tasks. */
  filter?: "overdue"
  /** Filter to tasks whose name contains this text. */
  search?: string
}

// ─────────────────────────── MCP types ───────────────────────────

export interface McpTool {
  name: string
  description: string
  inputSchema: unknown
}

export interface McpContent {
  type: string
  text: string
}

export interface McpCallResult {
  content: McpContent[]
  isError: boolean
}

export class OneCampClient {
  private readonly token: string
  private readonly baseUrl: string
  private readonly fetchImpl: typeof fetch
  private rpcId = 0

  constructor(opts: OneCampClientOptions) {
    if (!opts.token) throw new Error("OneCampClient: token is required")
    if (!opts.baseUrl) throw new Error("OneCampClient: baseUrl is required")
    this.token = opts.token
    this.baseUrl = opts.baseUrl.replace(/\/+$/, "")
    const f = opts.fetch ?? globalThis.fetch
    if (!f) {
      throw new Error("OneCampClient: no fetch implementation found; pass one via options.fetch")
    }
    this.fetchImpl = f.bind(globalThis)
  }

  private async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const res = await this.fetchImpl(`${this.baseUrl}/v1${path}`, {
      method,
      headers: {
        Authorization: `Bearer ${this.token}`,
        "Content-Type": "application/json",
      },
      body: body === undefined ? undefined : JSON.stringify(body),
    })
    const text = await res.text()
    let parsed: any = undefined
    if (text) {
      try {
        parsed = JSON.parse(text)
      } catch {
        parsed = text
      }
    }
    if (!res.ok) {
      const msg = (parsed && (parsed.msg || parsed.message)) || `request failed with ${res.status}`
      throw new OneCampError(res.status, String(msg), parsed)
    }
    return parsed as T
  }

  /** The identity (and scopes) behind the token. */
  async me(): Promise<Identity> {
    const res = await this.request<{ data: Identity }>("GET", "/me")
    return res.data
  }

  readonly tables = {
    list: async (): Promise<DataTable[]> => {
      const res = await this.request<{ data: DataTable[] }>("GET", "/tables")
      return res.data ?? []
    },
    get: async (tableId: string): Promise<TableBundle> => {
      const res = await this.request<{ data: TableBundle }>("GET", `/tables/${encodeURIComponent(tableId)}`)
      return res.data
    },
    createRow: async (
      tableId: string,
      values: Record<string, unknown>,
      position = 0,
    ): Promise<TableRow> => {
      const res = await this.request<{ data: TableRow }>(
        "POST",
        `/tables/${encodeURIComponent(tableId)}/rows`,
        { values, position },
      )
      return res.data
    },
    // aggregate computes a grouped totals/breakdown/trend over a table's rows
    // server-side (count/sum/avg/min/max, optional group-by + filters) without
    // downloading every row. Read-only; requires the tables:read scope.
    aggregate: async (
      tableId: string,
      query: AggregateQuery,
    ): Promise<AggregateResult> => {
      const res = await this.request<{ data: AggregateResult }>(
        "POST",
        `/tables/${encodeURIComponent(tableId)}/aggregate`,
        query,
      )
      return res.data
    },
  }

  readonly tasks = {
    list: async (opts: ListTasksOptions = {}): Promise<ToolResult> => {
      const q = new URLSearchParams()
      if (opts.status) q.set("status", opts.status)
      if (opts.filter) q.set("filter", opts.filter)
      if (opts.search) q.set("search", opts.search)
      const suffix = q.toString() ? `?${q.toString()}` : ""
      return this.request<ToolResult>("GET", `/tasks${suffix}`)
    },
    create: async (input: CreateTaskInput): Promise<ToolResult> => {
      return this.request<ToolResult>("POST", "/tasks", input)
    },
    updateStatus: async (taskId: string, status: TaskStatus): Promise<ToolResult> => {
      return this.request<ToolResult>("POST", `/tasks/${encodeURIComponent(taskId)}/status`, { status })
    },
  }

  readonly projects = {
    list: async (): Promise<ToolResult> => {
      return this.request<ToolResult>("GET", "/projects")
    },
  }

  readonly messages = {
    sendToChannel: async (channelUuid: string, text: string): Promise<ToolResult> => {
      return this.request<ToolResult>("POST", "/messages/channel", { channel_uuid: channelUuid, text })
    },
    sendDM: async (toUuid: string, text: string): Promise<ToolResult> => {
      return this.request<ToolResult>("POST", "/messages/dm", { to_uuid: toUuid, text })
    },
  }

  // ─────────────────── MCP server (JSON-RPC over /v1/mcp) ───────────────────

  private async rpc<T>(method: string, params?: unknown): Promise<T> {
    const id = ++this.rpcId
    const res = await this.request<{ result?: T; error?: { code: number; message: string } }>(
      "POST",
      "/mcp",
      { jsonrpc: "2.0", id, method, params },
    )
    if (res.error) {
      throw new OneCampError(200, res.error.message, res.error)
    }
    return res.result as T
  }

  readonly mcp = {
    /** List the tools this token can see and call. */
    listTools: async (): Promise<McpTool[]> => {
      const res = await this.rpc<{ tools: McpTool[] }>("tools/list")
      return res.tools ?? []
    },
    /** Call a tool by name with arguments. */
    callTool: async (name: string, args: Record<string, unknown> = {}): Promise<McpCallResult> => {
      return this.rpc<McpCallResult>("tools/call", { name, arguments: args })
    },
  }
}

export default OneCampClient
