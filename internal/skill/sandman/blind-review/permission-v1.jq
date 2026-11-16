# Native whole-value wildcard matching, without interpreting regex syntax.
def wildcard($pattern; $value):
  ($pattern | split("") | map(
    if . == "*" then ".*" elif . == "?" then "." else
      if test("[.\\\\+()\\[\\]{}^$|]") then "\\" + . else . end
    end) | join("")) as $regex |
  $value | test("^" + $regex + "$");
def allowed($rules; $action; $resource):
  [$rules[] |
   select(wildcard(.permission; $action) and wildcard(.pattern; $resource))] |
  last | .action == "allow";
