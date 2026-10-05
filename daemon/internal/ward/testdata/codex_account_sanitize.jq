# Reject unknown keys before reducing the response. Never copy arbitrary keys,
# account identifiers, error messages or token values to the host capture.
def keys_are($expected): type == "object" and (keys == ($expected | sort));
if (keys_are(["account", "requiresOpenaiAuth"]) and (.requiresOpenaiAuth | type == "boolean")) then
  if .account == null then
    {account:false, fields:{account:"null",requiresOpenaiAuth:"boolean"},plan:""}
  elif (.account | keys_are(["type", "email", "planType"])) and
       .account.type == "chatgpt" and (.account.email | (type == "string" or type == "null")) and
       (.account.planType | IN("free", "go", "plus", "pro", "prolite", "team", "self_serve_business_prolite", "self_serve_business_usage_based", "business", "ent26", "enterprise_cbp_automation", "enterprise_cbp_usage_based", "enterprise", "edu", "unknown")) then
    {account:true, fields:{account:"object",requiresOpenaiAuth:"boolean",
      "account.type":"string","account.email":(.account.email | type),"account.planType":"string"},plan:.account.planType}
  else error("unreviewed account shape") end
else error("unreviewed result shape") end
