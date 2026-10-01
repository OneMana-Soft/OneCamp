# OneCamp TypeScript SDK

A small, dependency-free client for the OneCamp public API (`/v1`). It authenticates with a scoped API token and runs as that token's owner, narrowed to the token's scopes.

## Install

```bash
npm install @onecamp/sdk
```

Requires Node 18+ (for the built-in `fetch`). In older runtimes, pass your own `fetch` implementation.

## Create a token

In OneCamp, go to Settings > API tokens, create a token, and grant only the scopes you need. The plaintext is shown once at creation.

## Quick start

```ts
import { OneCampClient } from "@onecamp/sdk"

const oc = new OneCampClient({
  token: process.env.ONECAMP_TOKEN!,
  baseUrl: "https://onecamp.example.com",
})

// Who am I, and what can this token do?
const me = await oc.me()
console.log(me.scopes)

// Tasks
const tasks = await oc.tasks.list({ status: "inProgress" })
const created = await oc.tasks.create({
  task_name: "Ship the SDK",
  project_uuid: "PROJECT_UUID",
  priority: "high",
})
await oc.tasks.updateStatus("TASK_UUID", "done")

// Projects
const projects = await oc.projects.list()

// Messages
await oc.messages.sendToChannel("CHANNEL_UUID", "Deploy finished ✅")
await oc.messages.sendDM("USER_UUID", "Can you review the PR?")

// Tables
const allTables = await oc.tables.list()
const bundle = await oc.tables.get("TABLE_UUID")
await oc.tables.createRow("TABLE_UUID", { [bundle.fields[0].id]: "Acme" })
```

## MCP

OneCamp is also a Model Context Protocol (MCP) server. The same token works as MCP auth at `POST /v1/mcp`. You can drive it through the SDK:

```ts
const tools = await oc.mcp.listTools()
const result = await oc.mcp.callTool("create_task", {
  task_name: "From MCP",
  project_uuid: "PROJECT_UUID",
})
console.log(result.content[0].text)
```

Or point any MCP-capable client (Claude Desktop, Cursor, a custom agent) at:

```
URL:   https://onecamp.example.com/v1/mcp
Auth:  Authorization: Bearer oc_...
```

The client sees only the tools the token's scopes allow.

## Scopes

`tasks:read`, `tasks:write`, `projects:read`, `projects:write`, `docs:read`, `docs:write`, `messages:read`, `messages:write`, `calendar:write`, `tables:read`, `tables:write`.

## Errors

Any non-2xx response throws `OneCampError` with `status`, `message`, and the raw `body`.

```ts
import { OneCampError } from "@onecamp/sdk"

try {
  await oc.tasks.create({ task_name: "x", project_uuid: "bad" })
} catch (e) {
  if (e instanceof OneCampError) console.error(e.status, e.message)
}
```
