.data | select(.info.id == $candidate and .info.location.directory == $directory and
  (.info.parentID // "") == "") |
([.messages[] | select(.type == "assistant")] | last) as $message |
[$message.content[]? | select(.type == "tool" and .name == "shell" and
  .state.status == "running" and (.state.input.command | type == "string") and
  (.state.input.command | contains($prompt_file)) and
  (.state.input.command | contains("spawn-opencode-v2.sh")))] as $calls |
select(($calls | length) == 1 and ($message.model.id | type == "string") and
  ($message.model.providerID | type == "string") and
  ($message.agent | type == "string") and ($calls[0].id | type == "string")) |
{id:.info.id,agent:$message.agent,model:$message.model,
 message_id:$message.id,call_id:$calls[0].id}
