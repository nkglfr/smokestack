/* smokestack — traduction côté client.
 *
 * Choix de la langue, dans l'ordre : paramètre ?lang=, choix mémorisé,
 * langues du navigateur, langue par défaut de l'instance, anglais.
 * Le serveur renvoie des dictionnaires déjà complétés par l'anglais,
 * donc une clé absente d'une traduction s'affiche en anglais, jamais
 * sous forme de clé brute.
 *
 * Marquage dans le HTML :
 *   <h1 data-i18n="home.faults"></h1>
 *   <input data-i18n-attr="placeholder:pairing.url">
 *   <p data-i18n="pairing.step2" data-i18n-vars='{"n":20}'></p>
 */
(function () {
  const S = { lang: "en", base: "en", def: "en", dict: {}, langs: [] };

  function remembered() {
    try { return localStorage.getItem("smokestack.lang"); } catch (e) { return null; }
  }
  function remember(code) {
    try { localStorage.setItem("smokestack.lang", code); } catch (e) {}
  }

  // Browser codes that should map to a shipped language.
  const ALIASES = { no: "nb", nn: "nb", "pt-br": "pt", "pt-pt": "pt" };

  function pick(available) {
    const has = c => available.some(l => l.code === c);
    const alias = c => ALIASES[(c || "").toLowerCase()];
    const q = new URLSearchParams(location.search).get("lang");
    if (q && has(q)) return q;
    const r = remembered();
    if (r && has(r)) return r;
    for (const nav of (navigator.languages || [navigator.language || ""])) {
      if (has(nav)) return nav;
      const short = nav.split("-")[0];
      if (has(short)) return short;
      const a = alias(nav) || alias(short);
      if (a && has(a)) return a;
    }
    return has(S.def) ? S.def : S.base;
  }

  function t(key, vars) {
    let s = S.dict[key];
    if (s == null) return key;
    if (vars) s = s.replace(/\{([a-z_]+)\}/g, (m, n) => vars[n] != null ? vars[n] : m);
    return s;
  }

  function apply(root) {
    const r = root || document;
    r.querySelectorAll("[data-i18n]").forEach(el => {
      let vars = null;
      if (el.dataset.i18nVars) { try { vars = JSON.parse(el.dataset.i18nVars); } catch (e) {} }
      el.textContent = t(el.dataset.i18n, vars);
    });
    r.querySelectorAll("[data-i18n-attr]").forEach(el => {
      el.dataset.i18nAttr.split(";").forEach(pair => {
        const [attr, key] = pair.split(":");
        if (attr && key) el.setAttribute(attr.trim(), t(key.trim()));
      });
    });
  }

  function date(ts, opts) {
    try {
      return new Intl.DateTimeFormat(S.lang, opts || {
        day: "2-digit", month: "short", year: "numeric",
        hour: "2-digit", minute: "2-digit"
      }).format(new Date(ts * 1000));
    } catch (e) { return new Date(ts * 1000).toISOString(); }
  }

  function ago(ts) {
    const m = Math.max(0, Math.round((Date.now() / 1000 - ts) / 60));
    if (m < 60) return t("common.minutes_ago", { n: m });
    if (m < 2880) return t("common.hours_ago", { n: Math.round(m / 60) });
    return t("common.days_ago", { n: Math.round(m / 1440) });
  }

  function num(v, digits) {
    if (v == null || isNaN(v)) return "—";
    try {
      return new Intl.NumberFormat(S.lang, {
        minimumFractionDigits: digits || 0, maximumFractionDigits: digits || 0
      }).format(v);
    } catch (e) { return Number(v).toFixed(digits || 0); }
  }

  async function init() {
    const meta = await fetch("/api/v1/i18n").then(r => r.json());
    S.base = meta.base; S.def = meta.default; S.langs = meta.languages || [];
    S.lang = pick(S.langs);
    S.dict = await fetch("/api/v1/i18n/" + encodeURIComponent(S.lang)).then(r => r.json());
    document.documentElement.lang = S.lang;
    document.documentElement.dir = S.dict["_meta.dir"] === "rtl" ? "rtl" : "ltr";
    apply();
    return S.lang;
  }

  async function set(code) {
    remember(code);
    const u = new URL(location.href);
    u.searchParams.delete("lang");
    history.replaceState(null, "", u.toString());
    await init();
    document.dispatchEvent(new CustomEvent("i18n:change", { detail: code }));
  }

  window.I18N = {
    init, t, apply, set, date, ago, num,
    get lang() { return S.lang; },
    get langs() { return S.langs; }
  };
})();
