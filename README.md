# cpa-claude-quota-scheduler

A CLIProxyAPI plugin that routes across multiple Claude OAuth subscription
seats by burn pace, while keeping each conversation pinned to one seat so
Anthropic prompt caches keep hitting.

Status: under construction. See AGENTS.md for the design contract.
