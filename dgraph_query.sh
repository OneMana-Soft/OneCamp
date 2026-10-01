#!/bin/bash
curl -s -X POST http://localhost:8080/query -H "Content-Type: application/json" -d '{"query": "{ q(func: has(Recording.egressId)) { Recording.egressId Recording.recordingObjKey } }"}'
