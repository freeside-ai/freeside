// responseTolerance: the contract's asymmetric skew policy (api/README.md,
// Response Tolerance). A client built from an older contract must keep
// decoding responses from a newer daemon, while the daemon keeps rejecting
// anything it doesn't know in a request.
//
// The rule runs once on the whole document (`given: $`) because whether a
// schema is a request or a response schema depends on where `paths` uses
// it, not on the schema itself. A schema is response-reachable when a
// response reaches it through any chain of `$ref`s, and request-reachable
// likewise from request bodies and parameters. A schema both reach counts
// as a response schema: the client decodes it, and the daemon's strict
// decoder still refuses unknown fields in it on the way in.
//
// functionOptions.check picks the rule:
//   responseOpen     a response-reachable object schema must not set
//                    additionalProperties: false.
//   requestClosed    a request-only object schema must set it.
//   responseUnknown  a response-reachable string enum of two or more
//                    values must list unknown, the member a client
//                    decodes a value it doesn't know as.
// An object schema whose additionalProperties holds a schema is a map and
// neither object rule applies to it. The property a oneOf discriminates on
// is exempt from the enum rule: a new union arm is a breaking change, not
// one a client can decode as unknown.

function getSchema() {
  return {
    name: "responseTolerance",
    description: "Response schemas tolerate added fields; request-only schemas reject them.",
  };
}

function resolvePointer(doc, ref) {
  if (typeof ref !== "string" || ref.indexOf("#/") !== 0) {
    return undefined;
  }
  var node = doc;
  var parts = ref.slice(2).split("/");
  for (var i = 0; i < parts.length; i++) {
    if (node === null || typeof node !== "object") {
      return undefined;
    }
    node = node[parts[i].replace(/~1/g, "/").replace(/~0/g, "~")];
  }
  return node;
}

// collectRefs appends every $ref string found anywhere under node.
function collectRefs(node, out) {
  if (Array.isArray(node)) {
    for (var i = 0; i < node.length; i++) {
      collectRefs(node[i], out);
    }
  } else if (node !== null && typeof node === "object") {
    for (var key in node) {
      if (key === "$ref" && typeof node[key] === "string") {
        out.push(node[key]);
      } else {
        collectRefs(node[key], out);
      }
    }
  }
}

// reachable returns the set of $ref targets reachable from the roots,
// following refs transitively.
function reachable(doc, roots) {
  var seen = {};
  var stack = [];
  collectRefs(roots, stack);
  while (stack.length > 0) {
    var ref = stack.pop();
    if (seen[ref]) {
      continue;
    }
    seen[ref] = true;
    collectRefs(resolvePointer(doc, ref), stack);
  }
  return seen;
}

function isMap(schema) {
  var ap = schema.additionalProperties;
  return ap !== null && typeof ap === "object";
}

// subschemas appends {path, schema, prop} for schema and every schema
// written inline under it, descending into properties, items,
// compositions, and map values. prop is the nearest property name, so an
// enum on a property's items still names the property.
function subschemas(schema, path, prop, out) {
  if (schema === null || typeof schema !== "object" || Array.isArray(schema)) {
    return;
  }
  out.push({ path: path, schema: schema, prop: prop });
  var props = schema.properties || {};
  for (var name in props) {
    subschemas(props[name], path + ".properties." + name, name, out);
  }
  subschemas(schema.items, path + ".items", prop, out);
  var compositions = ["allOf", "oneOf", "anyOf"];
  for (var c = 0; c < compositions.length; c++) {
    var members = schema[compositions[c]] || [];
    for (var i = 0; i < members.length; i++) {
      subschemas(members[i], path + "." + compositions[c] + "[" + i + "]", prop, out);
    }
  }
  if (isMap(schema)) {
    subschemas(schema.additionalProperties, path + ".additionalProperties", prop, out);
  }
}

function isObjectSchema(schema) {
  return (schema.type === "object" || !!schema.properties) && !isMap(schema);
}

// isOpenEnum reports a string enum of two or more non-null values: one a
// newer daemon could widen. A constant or a null marker can't widen.
function isOpenEnum(schema) {
  if (!Array.isArray(schema.enum)) {
    return false;
  }
  var values = 0;
  for (var i = 0; i < schema.enum.length; i++) {
    if (schema.enum[i] === null) {
      continue;
    }
    if (typeof schema.enum[i] !== "string") {
      return false;
    }
    values++;
  }
  return values >= 2;
}

// discriminated returns the set of "Schema.property" pairs that a oneOf
// discriminator names on the schemas it chooses between.
function discriminated(doc) {
  var exempt = {};
  var stack = [doc.components];
  while (stack.length > 0) {
    var node = stack.pop();
    if (node === null || typeof node !== "object") {
      continue;
    }
    if (node.discriminator && node.discriminator.propertyName && Array.isArray(node.oneOf)) {
      for (var i = 0; i < node.oneOf.length; i++) {
        var ref = node.oneOf[i] && node.oneOf[i].$ref;
        if (typeof ref === "string") {
          exempt[ref.split("/").pop() + "." + node.discriminator.propertyName] = true;
        }
      }
    }
    for (var key in node) {
      stack.push(node[key]);
    }
  }
  return exempt;
}

var METHODS = ["get", "put", "post", "delete", "options", "head", "patch", "trace"];

function runRule(doc) {
  var check = context.options && context.options.check;
  if (check !== "responseOpen" && check !== "requestClosed" && check !== "responseUnknown") {
    return [{
      message: "responseTolerance: functionOptions.check must be responseOpen, requestClosed, or responseUnknown",
    }];
  }
  var requestRoots = [];
  var responseRoots = [];
  var inline = [];
  var paths = doc.paths || {};
  for (var p in paths) {
    var item = paths[p] || {};
    requestRoots.push(item.parameters);
    for (var m = 0; m < METHODS.length; m++) {
      var op = item[METHODS[m]];
      if (!op) {
        continue;
      }
      var opPath = "$.paths['" + p + "']." + METHODS[m];
      requestRoots.push(op.parameters, op.requestBody);
      responseRoots.push(op.responses);
      inline.push({ path: opPath + ".requestBody", node: op.requestBody, response: false });
      inline.push({ path: opPath + ".responses", node: op.responses, response: true });
    }
  }
  var inRequest = reachable(doc, requestRoots);
  var inResponse = reachable(doc, responseRoots);

  var exempt = discriminated(doc);
  var candidates = [];
  var schemas = (doc.components && doc.components.schemas) || {};
  for (var name in schemas) {
    var ref = "#/components/schemas/" + name;
    var found = [];
    subschemas(schemas[name], "$.components.schemas." + name, null, found);
    for (var f = 0; f < found.length; f++) {
      found[f].owner = name;
      found[f].response = !!inResponse[ref];
      found[f].request = !!inRequest[ref];
      candidates.push(found[f]);
    }
  }
  // Schemas written inline in an operation take that position's direction.
  for (var n = 0; n < inline.length; n++) {
    var media = [];
    collectMediaSchemas(inline[n].node, inline[n].path, media);
    for (var s = 0; s < media.length; s++) {
      var nested = [];
      subschemas(media[s].schema, media[s].path, null, nested);
      for (var k = 0; k < nested.length; k++) {
        nested[k].response = inline[n].response;
        nested[k].request = !inline[n].response;
        candidates.push(nested[k]);
      }
    }
  }

  var results = [];
  for (var r = 0; r < candidates.length; r++) {
    var cand = candidates[r];
    if (check === "responseUnknown") {
      if (cand.response && isOpenEnum(cand.schema) && cand.schema.enum.indexOf("unknown") < 0 &&
        !exempt[cand.owner + "." + cand.prop]) {
        results.push({
          message: cand.path + " is a string enum a response reaches and must list unknown, " +
            "so an older client decodes a member a newer daemon adds",
          path: cand.path,
        });
      }
      continue;
    }
    if (!isObjectSchema(cand.schema)) {
      continue;
    }
    var closed = cand.schema.additionalProperties === false;
    if (check === "responseOpen" && cand.response && closed) {
      results.push({
        message: cand.path + " is reachable from a response and sets additionalProperties: false; " +
          "delete the line so an older client tolerates added fields",
        path: cand.path,
      });
    }
    if (check === "requestClosed" && cand.request && !cand.response && !closed) {
      results.push({
        message: cand.path + " is used only in requests and must set additionalProperties: false",
        path: cand.path,
      });
    }
  }
  return results;
}

// collectMediaSchemas finds the schema of each media type under a request
// body or a responses object written inline.
function collectMediaSchemas(node, path, out) {
  if (node === null || typeof node !== "object") {
    return;
  }
  if (node.content && typeof node.content === "object") {
    for (var ct in node.content) {
      var media = node.content[ct] || {};
      if (media.schema) {
        out.push({ path: path + ".content['" + ct + "'].schema", schema: media.schema });
      }
    }
    return;
  }
  for (var key in node) {
    if (key !== "$ref") {
      collectMediaSchemas(node[key], path + "['" + key + "']", out);
    }
  }
}
