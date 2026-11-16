# Select only the latest assistant message of an exact-worktree root. The
# unpredictable literal prompt path ties this selection to the running call.
select(.info.directory == $directory and (.info.parentID // "") == "") |
([.messages[] | select(.info.role == "assistant")] | last) as $message |
[$message.parts[]? |
 select(.type == "tool" and (.tool == "bash" or .tool == "shell") and
        .state.status == "running" and
        (.state.input.command | type == "string") and
        (.state.input.command | contains($prompt_file)) and
        (.state.input.command | contains("spawn-opencode-v1.sh")))] as $calls |
select(($calls | length) == 1 and
       ($calls[0].callID | type == "string") and
       ($message.info.agent | type == "string") and
       ($message.info.modelID | type == "string") and
       ($message.info.providerID | type == "string")) |
{id: .info.id, message_id: $message.info.id, call_id: $calls[0].callID,
 agent: $message.info.agent,
 model: {modelID: $message.info.modelID, providerID: $message.info.providerID},
 variant: $message.info.variant,
 permission: (.info.permission // [])}
