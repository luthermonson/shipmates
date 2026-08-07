export function escapeHTML(value) {
  return String(value)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;");
}

export function nowISO() {
  return new Date().toISOString().replace("T", " ").slice(0, 19);
}

export function shortModel(model) {
  return String(model).replace(/^claude-/, "").replace(/-\d{8}$/, "");
}

export function b64ToBytes(value) {
  const binary = atob(value);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
  return bytes;
}

export function humanSize(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(0)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

export function truncateName(name, max = 12) {
  if (name.length <= max) return name;
  const dot = name.lastIndexOf(".");
  if (dot > 0 && name.length - dot <= 6) {
    const ext = name.slice(dot);
    return name.slice(0, Math.max(1, max - ext.length - 1)) + "…" + ext;
  }
  return name.slice(0, max - 1) + "…";
}
