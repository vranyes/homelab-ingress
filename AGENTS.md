
<!-- gograph-start: do not remove -->
## Gograph — Go Repository Intelligence

Rules (enforced when gograph MCP server is connected):

1. NEVER use grep, rg, find, or glob to search for Go symbols, functions, structs,
   or types.
   Use gograph_query instead.
2. Before editing any symbol, run gograph_plan with symbol and with_context=true
   — this returns the change plan AND full context for every inspect_first symbol
   in one call.
3. After editing Go code, run gograph_review with uncommitted=true to verify test
   coverage and blast radius.
4. To understand a symbol, use gograph_context — it returns node, source, callers,
   callees, tests, and an architectural role classification in one call.
   Use uncommitted=true to get context for all uncommitted symbols at once.
5. Run gograph_capabilities at the start of any Go coding session.
   <!-- gograph-end: do not remove -->
