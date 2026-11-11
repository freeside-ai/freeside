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
//   responseOpen   a response-reachable object schema must not set
//                  additionalProperties: false.
//   requestClosed  a request-only object schema must set it.
// An object schema whose additionalProperties holds a schema is a map and
// neither rule applies to it.

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

// objectSchemas appends {path, schema} for every object schema at or under
// schema, descending into properties, items, compositions, and map values.
function objectSchemas(schema, path, out) {
  if (schema === null || typeof schema !== "object" || Array.isArray(schema)) {
    return;
  }
  var ap = schema.additionalProperties;
  var isMap = ap !== null && typeof ap === "object";
  if ((schema.type === "object" || schema.properties) && !isMap) {
    out.push({ path: path, schema: schema });
  }
  var props = schema.properties || {};
  for (var name in props) {
    objectSchemas(props[name], path + ".properties." + name, out);
  }
  objectSchemas(schema.items, path + ".items", out);
  var compositions = ["allOf", "oneOf", "anyOf"];
  for (var c = 0; c < compositions.length; c++) {
    var members = schema[compositions[c]] || [];
    for (var i = 0; i < members.length; i++) {
      objectSchemas(members[i], path + "." + compositions[c] + "[" + i + "]", out);
    }
  }
  if (isMap) {
    objectSchemas(ap, path + ".additionalProperties", out);
  }
}

var METHODS = ["get", "put", "post", "delete", "options", "head", "patch", "trace"];

function runRule(doc) {
  var check = context.options && context.options.check;
  if (check !== "responseOpen" && check !== "requestClosed") {
    return [{ message: "responseTolerance: functionOptions.check must be responseOpen or requestClosed" }];
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

  var candidates = [];
  var schemas = (doc.components && doc.components.schemas) || {};
  for (var name in schemas) {
    var ref = "#/components/schemas/" + name;
    var found = [];
    objectSchemas(schemas[name], "$.components.schemas." + name, found);
    for (var f = 0; f < found.length; f++) {
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
      objectSchemas(media[s].schema, media[s].path, nested);
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
