const ref = field => field.map ? `${field.map}[${JSON.stringify(field.name)}]` : field.name;

export const roleFields = model => (model.fields || []).filter(field => field.policy).map(field => ({
  ...field, type: field.logical_type || "string", ref: ref(field),
}));

export function roleOperators(type) {
  if (type === "string_array") return [
    { value: "contains", label: "contains" },
    { value: "not_contains", label: "does not contain" },
  ];
  if (type === "boolean") return [
    { value: "eq", label: "is" }, { value: "ne", label: "is not" },
  ];
  return [{ value: "eq", label: "equals" }, { value: "ne", label: "does not equal" }];
}

const quote = value => JSON.stringify(String(value));

export function conditionExpression(condition, field) {
  const fieldRef = field.ref || ref(field);
  let value;
  if (condition.source === "user") value = `user.${condition.value}`;
  else if (field.type === "boolean") value = condition.value === "false" ? "false" : "true";
  else value = quote(condition.value);
  if (condition.op === "contains" || condition.op === "not_contains") {
    const expression = `${value} in ${fieldRef}`;
    return condition.op === "not_contains" ? `!(${expression})` : expression;
  }
  return `${fieldRef} ${condition.op === "ne" ? "!=" : "=="} ${value}`;
}

export function buildRolePolicy(conditions, fields, match) {
  const byRef = new Map(fields.map(field => [field.ref, field]));
  const expressions = conditions.map(condition => {
    const field = byRef.get(condition.field);
    if (!field) throw new Error("Choose a trace field for every condition.");
    if (!condition.value && field.type !== "boolean") throw new Error("Choose a value for every condition.");
    return conditionExpression(condition, field);
  });
  if (!expressions.length) throw new Error("Add at least one condition.");
  return expressions.join(match === "any" ? " || " : " && ");
}

function parseValue(raw, field, attrs) {
  const user = raw.match(/^user\.([A-Za-z_][A-Za-z0-9_]*)$/);
  if (user && field.type === "string" && attrs.includes(user[1]))
    return { source: "user", value: user[1] };
  if (field.type === "boolean" && /^(true|false)$/.test(raw))
    return { source: "literal", value: raw };
  try {
    const value = JSON.parse(raw);
    if (typeof value === "string") return { source: "literal", value };
  } catch (_) { /* not a builder-supported literal */ }
  return null;
}

function parseCondition(expression, fields, attrs) {
  const byRef = new Map(fields.map(field => [field.ref, field]));
  const negativeContains = expression.match(/^!\((.+)\)$/);
  const contains = (negativeContains ? negativeContains[1] : expression).match(/^(.+)\s+in\s+(.+)$/);
  if (contains && byRef.get(contains[2])?.type === "string_array") {
    const field = byRef.get(contains[2]);
    const value = parseValue(contains[1], field, attrs);
    if (value?.source === "literal") return {
      field: field.ref, op: negativeContains ? "not_contains" : "contains", ...value,
    };
  }
  const comparison = expression.match(/^(.+?)\s*(==|!=)\s*(.+)$/);
  if (!comparison || !byRef.has(comparison[1])) return null;
  const field = byRef.get(comparison[1]);
  const value = parseValue(comparison[3], field, attrs);
  return value ? { field: field.ref, op: comparison[2] === "!=" ? "ne" : "eq", ...value } : null;
}

export function parseRolePolicies(policies, fields, attrs) {
  if (!policies?.length) return null;
  const expression = policies.map(policy => policy.expression).join(" || ");
  const separators = [...expression.matchAll(/\s+(&&|\|\|)\s+/g)].map(match => match[1]);
  if (new Set(separators).size > 1) return null;
  const match = separators[0] === "||" ? "any" : "all";
  const parts = expression.split(/\s+(?:&&|\|\|)\s+/);
  const conditions = parts.map(part => parseCondition(part.trim(), fields, attrs));
  return conditions.every(Boolean) ? { match, conditions } : null;
}

export function readablePolicy(expression) {
  return expression
    .replace(/!\(("(?:\\.|[^"])*") in ([^&|]+)\)/g, "$2 does not contain $1")
    .replace(/("(?:\\.|[^"])*") in ([^&|]+)/g, "$2 contains $1")
    .replace(/\s*==\s*/g, " matches ")
    .replace(/\s*!=\s*/g, " does not match ")
    .replace(/user\.([A-Za-z_][A-Za-z0-9_]*)/g, "user’s $1")
    .replace(/\s*&&\s*/g, " and ")
    .replace(/\s*\|\|\s*/g, " or ");
}
