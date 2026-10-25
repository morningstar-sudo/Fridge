// Hash router. A route is a pattern like "#/databases/:name" and a loader returning a page module.

const routes = [];

export function route(pattern, load, { public: isPublic = false, nav = "", admin = false } = {}) {
  const keys = [];
  const re = new RegExp(
    "^" +
      pattern.replace(/:[a-z]+/g, (m) => {
        keys.push(m.slice(1));
        return "([^/]+)";
      }) +
      "$",
  );
  routes.push({ re, keys, load, isPublic, nav, admin });
}

export function match(hash) {
  const path = hash.split("?")[0] || "#/";
  for (const r of routes) {
    const m = r.re.exec(path);
    if (m) return { route: r, params: Object.fromEntries(r.keys.map((k, i) => [k, decodeURIComponent(m[i + 1])])) };
  }
  return null;
}

export function query(hash) {
  const i = hash.indexOf("?");
  return new URLSearchParams(i < 0 ? "" : hash.slice(i + 1));
}

export function navigate(hash) {
  if (location.hash === hash) window.dispatchEvent(new HashChangeEvent("hashchange"));
  else location.hash = hash;
}
