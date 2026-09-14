const structuralRoles = ["trace_id", "timestamp", "span_id", "parent_span_id", "name"];

export const bindingColumns = binding => structuralRoles
  .map(role => binding?.[role])
  .filter(Boolean);

export const fieldId = field => JSON.stringify([field.map || "", field.name]);
export const fieldRef = field => field.map
  ? `${field.map}[${JSON.stringify(field.name)}]`
  : field.name;

export const filterableFields = schema => (schema.fields || [])
  .filter(field => Array.isArray(field.operators) && field.operators.length);

const operatorLabels = {
  eq: "==", ne: "!=", in: "in list", not_in: "not in list", prefix: "startsWith()",
  exists: "exists()", not_exists: "!exists()", has: "element in array",
  has_any: "hasAny()", has_all: "hasAll()", gt: ">", gte: ">=", lt: "<", lte: "<=",
  between: "between()",
};
export const operatorLabel = op => operatorLabels[op] || op;

// ClickHouse reports the physical type during discovery. Setup displays the corresponding
// LightShip type as read-only so an operator never has to translate the database schema by hand.
export function logicalTypeForClickHouse(raw) {
  let type = String(raw || "").trim();
  let previous;
  do {
    previous = type;
    type = type.replace(/^(?:Nullable|LowCardinality)\((.*)\)$/, "$1").trim();
  } while (type !== previous);
  if (/^Array\(/.test(type)) return "string_array";
  if (/^(?:Bool|Boolean)$/i.test(type)) return "boolean";
  if (/^(?:U?Int\d*|Float\d*|Decimal(?:32|64|128|256)?\b)/i.test(type)) return "number";
  return "string";
}

// The trace list needs only these structural columns. Filters are evaluated in ClickHouse, so
// returning their physical columns would duplicate data and can expose an entire large map column.
export function queryColumns(schema) {
  return [...new Set(bindingColumns(schema.binding))];
}

export function conditionFromDraft(field, op, raw) {
  const condition = {name: field.name, op};
  if (field.map) condition.map = field.map;
  if (op === "exists" || op === "not_exists") return condition;

  const usesValues = op === "has_any" || op === "has_all" || op === "between" ||
    ((op === "in" || op === "not_in") && field.type === "string");
  if (usesValues) {
    const values = String(raw).split(",").map(v => v.trim()).filter(Boolean)
      .map(v => typedValue(v, field.type === "string_array" ? "string" : field.type));
    if (!values.length) throw new Error(`${fieldRef(field)} ${op} needs a value`);
    if (op === "between" && values.length !== 2)
      throw new Error(`${fieldRef(field)} between needs two comma-separated numbers`);
    condition.values = values;
    return condition;
  }
  condition.value = typedValue(raw, field.type === "string_array" ? "string" : field.type);
  return condition;
}

function typedValue(raw, type) {
  if (type === "number") {
    const value = Number(raw);
    if (String(raw).trim() === "" || !Number.isFinite(value))
      throw new Error("enter a number");
    return value;
  }
  if (type === "boolean") {
    if (raw === true || raw === "true") return true;
    if (raw === false || raw === "false") return false;
    throw new Error("choose true or false");
  }
  return String(raw);
}

export function policyOperators(type) {
  if (type === "string") return ["==", "!=", "in [ … ]"];
  if (type === "string_array") return ["element in array"];
  if (type === "boolean") return ["==", "!="];
  return [];
}

export function samplePolicyExpression(field, userAttributes = []) {
  const ref = fieldRef(field);
  if (field.type === "string_array") return `"agent:pii" in ${ref}`;
  if (field.type === "boolean") return `${ref} == true`;
  if (field.type === "string" && userAttributes.length)
    return `${ref} == user.${userAttributes[0]}`;
  if (field.type === "string") return `${ref} == "agent:pii"`;
  return "numbers are filter-only for now";
}
