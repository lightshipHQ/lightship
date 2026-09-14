import { $, session } from "./core.js";

const localValue = date => {
  const part = value => String(value).padStart(2, "0");
  return `${date.getFullYear()}-${part(date.getMonth() + 1)}-${part(date.getDate())}` +
    `T${part(date.getHours())}:${part(date.getMinutes())}`;
};

export function initDateRange(presetID, fromID, toID, customID) {
  const preset = $(presetID), from = $(fromID), to = $(toID), custom = $(customID);
  const showCustom = () => { custom.hidden = preset.value !== "custom"; };
  const applyPreset = () => {
    if (preset.value !== "custom" && preset.value !== "demo") {
      const hours = Number(preset.value);
      const now = new Date();
      to.value = localValue(now);
      from.value = localValue(new Date(now.getTime() - hours * 3600 * 1000));
    }
    showCustom();
  };
  preset.onchange = applyPreset;
  const markCustom = () => { preset.value = "custom"; showCustom(); };
  from.onchange = markCustom;
  to.onchange = markCustom;
  if (!from.value || !to.value) applyPreset();
  else showCustom();
  return applyPreset;
}

export function dateRange(fromID, toID, presetID = "") {
  const preset = presetID ? $(presetID) : null;
  if (preset?.value === "demo") {
    if (!session.demoWindow) throw new Error("demo dataset range is unavailable");
    return {...session.demoWindow};
  }
  if (preset && preset.value !== "custom") {
    const hours = Number(preset.value);
    const end = new Date();
    const start = new Date(end.getTime() - hours * 3600 * 1000);
    return { from: start.toISOString(), to: end.toISOString() };
  }
  const from = $(fromID).value, to = $(toID).value;
  if (!from || !to) throw new Error("choose both a start and end time");
  const start = new Date(from), end = new Date(to);
  if (!(start < end)) throw new Error("start time must be before end time");
  return { from: start.toISOString(), to: end.toISOString() };
}
