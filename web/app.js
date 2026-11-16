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

  const { html, url } = H;

  // A flag is the pair of regional indicator symbols for the country the
  // language declares in _meta.flag: no image to serve, and no table of
  // countries to keep in step with the language files. A platform without
  // flag glyphs renders the pair as the two letters of the country, which
  // is why the button shows the flag alone and the code only without one.
  const flag = cc => /^[A-Z]{2}$/.test(cc || "")
    ? String.fromCodePoint(...Array.from(cc, c => 0x1f1e6 + c.charCodeAt(0) - 65))
    : "";

  let SITE = {}, VERSION = null;

  // Whether the menu lists a page: no Federation tab leading to an empty
  // page on an instance that does not federate, and none at all for a page
  // the operator chose to hide.
  function shows(id) {
    const on = SITE.pages || {};
    return on[id] === undefined || !!on[id];
  }

  // Whether the operator took a page off the site. Links from other pages
  // follow this alone: the menu ignores an AS number set only in the
  // federation configuration, and the page answers all the same.
  function hides(id) {
    return (SITE.hidden_pages || []).includes(id);
  }

  function pages() {
    return PAGES.filter(p => shows(p.id));
  }

  function header(active) {
    const t = I18N.t;
    // aria-current spells "false" rather than being left off: the
    // stylesheet reads "page" and nothing else, and an attribute that is
    // always present is one fewer branch here.
    const nav = pages().map(p =>
      html`<a href="${p.href}" aria-current="${p.id === active ? "page" : "false"}">${t(p.key)}</a>`);
    // Under 760 px the links no longer fit: they move into a menu opened
    // by the button, instead of being cut off and unreachable.
    return html`<div class="wrap"><div class="hdr-in">
      <a class="brand" href="/"><span class="mark"></span><span>${SITE.title || "smokestack"}</span></a>
      <nav class="nav nav-wide">${nav}</nav>
      <span class="spacer"></span>${SITE.asn && html`<span class="chip mono hide-m">${SITE.asn}</span>`}
      ${themeButton()}${langPicker()}
      <button class="burger" id="burger" aria-label="${t("nav.menu")}" aria-expanded="false" aria-controls="navm">
        <span></span><span></span><span></span></button>
    </div><nav class="nav-menu" id="navm" hidden>${nav}</nav></div>`;
  }

  // Three states rather than two: a visitor whose system is dark may
  // still want this page light, and "auto" has to stay reachable once
  // one of the two has been chosen. The glyph carries a text-presentation
  // selector so a platform does not turn it into an emoji.
  const THEME_GLYPH = { auto: "\u25d0", light: "\u2600\ufe0e", dark: "\u263e\ufe0e" };

  function themeButton() {
    const t = I18N.t, pref = Theme.pref;
    return html`<button class="thm" id="thmBtn" aria-label="${t("nav.theme")}"
            title="${t("nav.theme_" + pref)}">${THEME_GLYPH[pref]}</button>`;
  }

  // Ten language names took a quarter of the header and said nothing a
  // visitor reads twice. The flag of the current language opens the list,
  // where the native name stays next to each flag: a flag alone names no
  // language, and several of them stand for more than one.
  function langPicker() {
    const t = I18N.t;
    const cur = I18N.langs.find(l => l.code === I18N.lang) || { code: I18N.lang, name: I18N.lang };
    const face = l => {
      const f = flag(l.flag);
      return f ? html`<span class="flag">${f}</span>`
               : html`<span class="lang-code">${String(l.code).toUpperCase()}</span>`;
    };
    const items = I18N.langs.map(l => html`<li role="none"><button type="button"
      role="menuitemradio" data-lang="${l.code}"
      aria-checked="${l.code === I18N.lang ? "true" : "false"}">${face(l)}<span>${l.name}</span>${
      l.coverage < 1 && html`<span class="faint">${Math.round(l.coverage * 100)} %</span>`}</button></li>`);
    return html`<div class="lang">
      <button class="lang-btn" id="langBtn" aria-haspopup="true" aria-expanded="false"
              aria-controls="langm" aria-label="${t("nav.language")}" title="${cur.name}">
        ${face(cur)}<span class="caret" aria-hidden="true">\u25be</span>
      </button>
      <ul class="lang-menu" id="langm" role="menu" aria-label="${t("nav.language")}"
          hidden>${items}</ul>
    </div>`;
  }

  // Le lien vers le site officiel vient de la constante OfficialURL du
  // binaire (/api/v1/version) : il est ecrit en dur et ne se configure
  // pas depuis le back-office.
  function footer() {
    const t = I18N.t, official = url((VERSION || {}).official_url);
    return html`<div class="wrap">
      <div class="fbottom">
        ${official ? html`<a href="${official}" rel="noopener" target="_blank">${t("footer.powered")}</a>`
                   : html`<span>${t("footer.powered")}</span>`}
      </div>
    </div>`;
  }

  function paintChrome(active) {
    H.render(document.getElementById("hdr"), header(active));
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
    H.render(document.getElementById("ftr"), footer());
    wireLang();
    const thm = document.getElementById("thmBtn");
    if (thm) {
      thm.onclick = () => Theme.set(Theme.next());
    }
  }

  // The header is rebuilt on every language change, so the listeners the
  // open menu needs on the document are added when it opens and removed
  // when it closes, rather than piling up one set per repaint.
  function wireLang() {
    const btn = document.getElementById("langBtn"), menu = document.getElementById("langm");
    if (!btn || !menu) return;
    const items = () => Array.from(menu.querySelectorAll("button"));

    const onAway = e => { if (!menu.contains(e.target) && !btn.contains(e.target)) close(); };
    const onKey = e => {
      if (e.key === "Escape") { close(); btn.focus(); return; }
      if (e.key !== "ArrowDown" && e.key !== "ArrowUp") return;
      e.preventDefault();
      const list = items(), at = list.indexOf(document.activeElement);
      const step = e.key === "ArrowDown" ? 1 : -1;
      list[(Math.max(at, 0) + step + list.length) % list.length].focus();
    };

    function close() {
      menu.hidden = true;
      btn.setAttribute("aria-expanded", "false");
      document.removeEventListener("click", onAway);
      document.removeEventListener("keydown", onKey);
    }
    function open() {
      menu.hidden = false;
      btn.setAttribute("aria-expanded", "true");
      document.addEventListener("click", onAway);
      document.addEventListener("keydown", onKey);
      (menu.querySelector('[aria-checked="true"]') || items()[0]).focus();
    }

    btn.onclick = e => { e.stopPropagation(); menu.hidden ? open() : close(); };
    btn.onkeydown = e => { if (e.key === "ArrowDown" && menu.hidden) { e.preventDefault(); open(); } };
    menu.onclick = e => {
      const item = e.target.closest("button[data-lang]");
      if (!item) return;
      close();
      I18N.set(item.dataset.lang);
    };
  }

  const pause = ms => new Promise(done => setTimeout(done, ms));

  // The longest a retry will ever wait, whatever the server asks for.
  const RETRY_CAP_S = 30;

  // getJSON tries again when the failure is one that passes — the network,
  // a 429, a 5xx while the service restarts — and throws once it persists.
  async function getJSON(url, tries = 4) {
    for (let i = 1; ; i++) {
      let r;
      try {
        r = await fetch(url);
      } catch (e) {
        if (i >= tries) throw e;
        await pause(1000 * 2 ** (i - 1));
        continue;
      }
      if (r.ok) return r.json();
      if (i >= tries || (r.status !== 429 && r.status < 500)) throw new Error(`${url}: HTTP ${r.status}`);
      // Retry-After is honoured, but capped: it is a number this page does
      // not choose, and an hour of it would leave the page sitting on
      // "loading" with nothing said. Only a positive number is taken — the
      // header also allows an HTTP date, which gives NaN, and a zero or a
      // negative value would otherwise skip the wait altogether and spend
      // every attempt at once.
      const after = +r.headers.get("Retry-After");
      await pause(1000 * (after > 0 ? Math.min(after, RETRY_CAP_S) : 2 ** (i - 1)));
    }
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
    document.addEventListener("theme:change", run);
    run();
  }

  // esc reste exporte tant qu'une page assemble encore ses chaines a la
  // main ; escURL n'avait plus d'appelant.
  window.App = { boot, getJSON, shows, hides, esc: H.esc, get site() { return SITE; } };
})();
