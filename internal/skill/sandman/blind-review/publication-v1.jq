# Pure report-to-publication transform. Keep it aligned with the publication
# contract; the parity tests include the approving-report corpus.
def trim: gsub("^\\s+|\\s+$"; "");
def token:
  trim | gsub("^[*_` .]+|[*_` .]+$"; "") | ascii_upcase |
  if . == "APPROVED" then "approved"
  elif . == "CHANGES_REQUESTED" then "changes_requested"
  else "" end;
def heading:
  if test("^(Decision|Findings)($|[:\\-\\t ])"; "i") then
    capture("^(?<name>Decision|Findings)(?<rest>.*)$"; "i") |
    {name: (.name | ascii_downcase), inline: (.rest | sub("^[:\\-\\t ]+"; "") | trim)}
  else {name: ., inline: ""} end;
def severity:
  "^(?:#{1,6}|[-*+]|[0-9]+[.)])?[ \\t]*[*_]*(?:blocking|important)\\b";
def empty_finding:
  ascii_downcase | trim |
  until(. == (gsub("^[*_:\\-–—/()\\[\\] \\t.]+|[*_:\\-–—/()\\[\\] \\t.,]+$"; "") |
    sub("^(blocking|important|findings|finding|issues|issue|items|item|or|and)($|(?=[ :\\-–—/()*_.,]))"; "") |
    gsub("^[*_:\\-–—/()\\[\\] \\t.,]+|[*_:\\-–—/()\\[\\] \\t.,]+$"; ""));
    gsub("^[*_:\\-–—/()\\[\\] \\t.]+|[*_:\\-–—/()\\[\\] \\t.,]+$"; "") |
    reduce ["blocking","important","findings","finding","issues","issue","items","item","or","and"][] as $word (.;
      sub("^" + $word + "($|(?=[ :\\-–—/()*_.,]))"; "")) |
    gsub("^[*_:\\-–—/()\\[\\] \\t.,]+|[*_:\\-–—/()\\[\\] \\t.,]+$"; "")) |
  IN("none", "n/a", "0", "", "no", "nothing");
def decision_verdict:
  reduce (split("\n")[] | trim) as $line
    ({verdict:"", decisions:0, contradicted:false, section:"", pending:false, group:false, level:0};
    if $line | startswith("#") then
      ($line | capture("^(?<marks>#+)(?<text>.*)$")) as $h |
      ($h.text | trim) as $text |
      ($text | heading) as $heading |
      if .section == "findings" and ($text | test(severity; "i")) then .group = true
      elif .section == "findings" and ($h.marks | length) > .level and $heading.name != "decision" then
        if .group then .contradicted = true | .group = false else . end
      else
        if .group then .contradicted = true else . end |
        .section = "" | .pending = false | .group = false |
        if $heading.name == "decision" then
          .section = "decision" | .decisions += 1 |
          if $heading.inline != "" then .verdict = ($heading.inline | token) else .pending = true end
        elif $heading.name == "findings" then .section = "findings" | .level = ($h.marks | length)
        else . end
      end
    elif $line == "" then .
    elif .section == "decision" and .pending then .verdict = ($line | token) | .pending = false
    elif .section == "findings" then
      if .group then
        .group = false | if ($line | empty_finding) then . else .contradicted = true end
      elif $line | test(severity; "i") then
        ($line | sub(severity; ""; "i")) as $rest |
        if ($rest | gsub("^[*_:\\-–— \\t]+|[*_:\\-–— \\t]+$"; "")) == "" then .group = true
        elif ($rest | empty_finding) then . else .contradicted = true end
      else . end
    else . end) |
  if .decisions != 1 or (.verdict == "approved" and (.contradicted or .group)) then "" else .verdict end;
gsub("/sandman"; "sandman"; "i") |
gsub("(?m)^[ \\t]*<!-- sandman-publication:[0-9a-f]{64} -->[ \\t]*$"; "") |
gsub("(?m)^[ \\t]*<!-- sandman-verdict:(approved|changes_requested) -->[ \\t]*$"; "") |
. as $decision |
($decision | decision_verdict) as $verdict |
($decision | sub("\n+$"; "")) + "\n\n" +
(if $verdict == "" then "" else "<!-- sandman-verdict:" + $verdict + " -->\n" end) +
"<!-- sandman-publication:" + $identity + " -->\n"
