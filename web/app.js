/* smokestack — gabarit commun des pages publiques.
 * Chaque page appelle App.boot("federation", render) : l'en-tête, le
 * pied de page et la langue sont posés, puis render() est rappelée à
 * chaque changement de langue.
 */
(function () {
  const PAGES = [
    { id: "home",       href: "/",           key: "nav.home" },
    { id: "federation", href: "/federation", key: "nav.federation" },
    { id: "network",    href: "/network",    key: "nav.network" },
    { id: "pairing",    href: "/pairing",    key: "nav.pairing" },
    { id: "about",      href: "/about",      key: "nav.about" }
  ];

  const esc = s => String(s == null ? "" : s).replace(/[&<>"']/g,
    c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

  let SITE = {}, VERSION = null;

  // Pages the instance says are relevant: no Federation tab leading to an
  // empty page on an instance that does not federate.
  function pages() {
    const on = (SITE.pages || {});
    return PAGES.filter(p => on[p.id] === undefined || on[p.id]);
  }

  function header(active) {
    const t = I18N.t;
    const nav = pages().map(p =>
      `<a href="${p.href}" ${p.id === active ? 'aria-current="page"' : ""}>${esc(t(p.key))}</a>`).join("");
    const opts = I18N.langs.map(l =>
      `<option value="${esc(l.code)}" ${l.code === I18N.lang ? "selected" : ""}>${esc(l.name)}` +
      (l.coverage < 1 ? ` · ${Math.round(l.coverage * 100)} %` : "") + `</option>`).join("");
    const asn = SITE.asn ? `<span class="chip mono hide-m">${esc(SITE.asn)}</span>` : "";
    // Under 760 px the links no longer fit: they move into a menu opened
    // by the button, instead of being cut off and unreachable.
    return `<div class="wrap"><div class="hdr-in">
      <a class="brand" href="/"><span class="mark"></span><span>${esc(SITE.title || "smokestack")}</span></a>
      <nav class="nav nav-wide">${nav}</nav>
      <span class="spacer"></span>${asn}
      <select class="lang-select" id="langSel" aria-label="${esc(t("nav.language"))}">${opts}</select>
      <button class="burger" id="burger" aria-label="${esc(t("nav.menu"))}" aria-expanded="false" aria-controls="navm">
        <span></span><span></span><span></span></button>
    </div><nav class="nav-menu" id="navm" hidden>${nav}</nav></div>`;
  }

  // Le lien vers le site officiel vient de la constante OfficialURL du
  // binaire (/api/v1/version) : il est ecrit en dur et ne se configure
  // pas depuis le back-office.
  function footer() {
    const t = I18N.t, V = VERSION || {};
    const asn = String(SITE.asn || "").replace(/\D/g, "");
    const link = (href, label, ext) => href
      ? `<a href="${esc(href)}"${ext ? ' rel="noopener" target="_blank"' : ""}>${esc(label)}</a>` : "";
    const col = (title, items) => {
      const li = items.filter(Boolean).map(x => `<li>${x}</li>`).join("");
      return li ? `<div class="fcol"><h4>${esc(title)}</h4><ul>${li}</ul></div>` : "";
    };
    const official = V.official_url || "";
    return `<div class="wrap">
      <div class="fgrid">
        <div class="fcol fbrand">
          <a class="brand" href="/"><span class="mark"></span><span>${esc(SITE.title || "smokestack")}</span></a>
          <p>${esc(SITE.org || "")}${SITE.asn ? " · " + esc(SITE.asn) : ""}</p>
          ${SITE.location ? `<p class="faint">${esc(SITE.location)}</p>` : ""}
        </div>
        ${col(t("footer.pages"), [
          link("/", t("footer.graphs")),
          ...pages().filter(p => p.id !== "home" && p.id !== "about")
            .map(p => link(p.href, t("footer." + p.id))),
          link("/about", t("footer.about"))])}
        ${col(t("footer.operator"), [
          link(SITE.url, t("footer.website"), 1),
          SITE.noc_email ? link("mailto:" + SITE.noc_email, t("footer.noc")) : "",
          link(SITE.peeringdb, t("footer.peeringdb"), 1),
          asn ? link("https://stat.ripe.net/AS" + asn, t("footer.ripe"), 1) : "",
          asn ? link("https://bgp.tools/as/" + asn, t("footer.bgptools"), 1) : ""])}
        ${col(t("footer.data"), [
          link("/api/v1/overview", t("footer.api")),
          link("/healthz", t("footer.health")),
          link("/api/v1/version", "/api/v1/version")])}
        ${col(t("footer.software"), [
          link(official, t("footer.official_site"), 1),
          link(V.repo_url, t("footer.source"), 1),
          V.version ? `<span class="faint">${esc(t("footer.version", { v: V.version }))}</span>` : ""])}
      </div>
      <div class="fbottom">
        <span>© ${new Date().getFullYear()} ${esc(SITE.org || "")} — ${esc(t("footer.rights"))}</span>
        <span class="spacer"></span>
        ${official ? `<a href="${esc(official)}" rel="noopener" target="_blank">${esc(t("footer.powered"))}</a>`
                   : `<span>${esc(t("footer.powered"))}</span>`}
      </div>
    </div>`;
  }

  function paintChrome(active) {
    document.getElementById("hdr").innerHTML = header(active);
    const burger = document.getElementById("burger"), menu = document.getElementById("navm");
    if (burger && menu) {
      const setOpen = open => {
        menu.hidden = !open;
        burger.setAttribute("aria-expanded", open ? "true" : "false");
        burger.classList.toggle("open", open);
      };
      burger.onclick = e => { e.stopPropagation(); setOpen(menu.hidden); };
      document.addEventListener("click", e => { if (!menu.hidden && !menu.contains(e.target)) setOpen(false); });
      document.addEventListener("keydown", e => { if (e.key === "Escape") setOpen(false); });
      addEventListener("resize", () => { if (innerWidth > 760) setOpen(false); });
    }
    document.getElementById("ftr").innerHTML = footer();
    document.getElementById("langSel").onchange = e => I18N.set(e.target.value);
  }

  async function boot(active, render) {
    const [site, ver] = await Promise.all([
      fetch("/api/v1/site").then(r => r.json()).catch(() => ({})),
      fetch("/api/v1/version").then(r => r.json()).catch(() => null)
    ]);
    SITE = site || {}; VERSION = ver;
    await I18N.init();
    const run = () => { paintChrome(active); I18N.apply(); render && render(SITE); };
    document.addEventListener("i18n:change", run);
    run();
  }

  window.App = { boot, esc, get site() { return SITE; } };
})();
