# Reduce a rate-limit read result ($kind == "read"), one updated
# notification's params ("updated"), or a list of reduced notifications
# ("merge") to field names and JSON types. Unknown keys and unreviewed enum
# values fail instead of being dropped. No usage value, limit name, credit
# identifier, balance, or error text is ever copied to the host capture.
def only($allowed): type == "object" and ((keys - $allowed) == []);
def kind: if type == "number" and . == floor then "integer" else type end;
def leaf($p; $k; $allowed):
  (if has($k) then (.[$k] | kind) else "absent" end) as $t
  | if any($allowed[]; . == $t) then {($p + "." + $k): $t} else error("unreviewed type") end;
def enum($p; $k; $values):
  if has($k) | not then {($p + "." + $k): "absent"}
  elif .[$k] == null then {($p + "." + $k): "null"}
  else .[$k] as $v
    | if any($values[]; . == $v) then {($p + "." + $k): "string"} else error("unreviewed enum") end
  end;
def member($p; $k; f): if has($k) then (.[$k] | f) else {($p + "." + $k): "absent"} end;
def plans: ["free", "go", "plus", "pro", "prolite", "team", "self_serve_business_prolite", "self_serve_business_usage_based", "business", "ent26", "enterprise_cbp_automation", "enterprise_cbp_usage_based", "enterprise", "edu", "unknown"];
def reached: ["rate_limit_reached", "workspace_owner_credits_depleted", "workspace_member_credits_depleted", "workspace_owner_usage_limit_reached", "workspace_member_usage_limit_reached"];
def window($p):
  if . == null then {($p): "null"}
  elif only(["resetsAt", "usedPercent", "windowDurationMins"]) then
    {($p): "object"} + leaf($p; "usedPercent"; ["integer"])
    + leaf($p; "windowDurationMins"; ["integer", "null", "absent"])
    + leaf($p; "resetsAt"; ["integer", "null", "absent"])
  else error("unreviewed window") end;
def credits($p):
  if . == null then {($p): "null"}
  elif only(["balance", "hasCredits", "unlimited"]) then
    {($p): "object"} + leaf($p; "hasCredits"; ["boolean"]) + leaf($p; "unlimited"; ["boolean"])
    + leaf($p; "balance"; ["string", "null", "absent"])
  else error("unreviewed credits") end;
def individual($p):
  if . == null then {($p): "null"}
  elif only(["limit", "remainingPercent", "resetsAt", "used"]) then
    {($p): "object"} + leaf($p; "limit"; ["string"]) + leaf($p; "used"; ["string"])
    + leaf($p; "remainingPercent"; ["integer"]) + leaf($p; "resetsAt"; ["integer"])
  else error("unreviewed individual limit") end;
def snapshot($p):
  if only(["credits", "individualLimit", "limitId", "limitName", "planType", "primary", "rateLimitReachedType", "secondary", "spendControlReached"]) then
    {($p): "object"}
    + leaf($p; "limitId"; ["string", "null", "absent"])
    + leaf($p; "limitName"; ["string", "null", "absent"])
    + leaf($p; "spendControlReached"; ["boolean", "null", "absent"])
    + enum($p; "planType"; plans)
    + enum($p; "rateLimitReachedType"; reached)
    + member($p; "primary"; window($p + ".primary"))
    + member($p; "secondary"; window($p + ".secondary"))
    + member($p; "credits"; credits($p + ".credits"))
    + member($p; "individualLimit"; individual($p + ".individualLimit"))
  else error("unreviewed snapshot") end;
def resetCredits($p):
  if . == null then {($p): "null"}
  elif only(["availableCount", "credits"]) and
       (.credits == null or ((.credits | type) == "array" and
         all(.credits[]; only(["description", "expiresAt", "grantedAt", "id", "resetType", "status", "title"])))) then
    {($p): "object"} + leaf($p; "availableCount"; ["integer"]) + leaf($p; "credits"; ["array", "null", "absent"])
  else error("unreviewed reset credits") end;
def buckets($p):
  if . == null then {($p): "null"}
  elif type == "object" then {($p): "object"}, (.[] | snapshot($p + ".*"))
  else error("unreviewed limit buckets") end;
# A field several buckets or notifications report with different types keeps
# every observed type, sorted and joined with "|".
def union(maps):
  reduce maps as $m ({}; reduce ($m | to_entries[]) as $e (.; .[$e.key] = ((.[$e.key] // []) + ($e.value | split("|")) | unique)))
  | map_values(join("|"));
def top($k; f): if has($k) then (.[$k] | f) else {($k): "absent"} end;

if $kind == "merge" then
  if type == "array" and all(.[]; only(["fields"]) and (.fields | type) == "object") then
    {updated: length, updatedFields: union(.[].fields)}
  else error("unreviewed merge input") end
elif $kind == "updated" then
  if only(["rateLimits"]) and has("rateLimits") then {fields: union(.rateLimits | snapshot("rateLimits"))}
  else error("unreviewed notification shape") end
elif $kind == "read" then
  if only(["rateLimitResetCredits", "rateLimits", "rateLimitsByLimitId"]) and has("rateLimits") then
    {
      usage: true,
      fields: union(
        (.rateLimits | snapshot("rateLimits")),
        top("rateLimitsByLimitId"; buckets("rateLimitsByLimitId")),
        top("rateLimitResetCredits"; resetCredits("rateLimitResetCredits"))),
      buckets: ((.rateLimitsByLimitId // {}) | length),
      codexBucket: ((.rateLimitsByLimitId // {}) | has("codex")),
      plan: (.rateLimits.planType // "")
    }
  else error("unreviewed result shape") end
else error("unknown kind") end
