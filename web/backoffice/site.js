/* smokestack back-office — publisher page.
 * Charge par admin.html, dans l'ordre : ces fichiers partagent une
 * portee globale, comme app.js et i18n.js du site public. */
/* ---------------------------------------------------- publisher page */
async function viewSite(m){
  H.render(m,html`<h2>Publisher page</h2>
    <p class="lead">This information feeds the public <a href="/about" target="_blank">/about</a> page.
      The NOC phone number never appears there: it is only returned to authenticated calls.</p>
    <div id="s">Loading…</div>`);
  try{
    const s=await api("GET","/api/v1/site");
    LANGS=(await api("GET","/api/v1/i18n").catch(()=>({languages:[]}))).languages;
    const f=(id,lab,val,ph)=>html`<div class="field"><label>${lab}</label>
      <input id="${id}" value="${val||""}" placeholder="${ph||""}"></div>`;
    H.render($("#s"),html`<div class="card"><h3>Identity</h3><div class="body"><div class="row2">
      ${f("title","Instance title",s.title)}
      ${f("org","Organisation",s.org)}
      ${f("asn","AS number",s.asn,"AS64500")}
      ${f("owner","Maintainer",s.owner)}
      ${f("location","Location",s.location,"Paris, FR")}
      ${f("timezone","Time zone",s.timezone,"Europe/Paris")}
      <div class="field"><label>Default language of public pages</label>
        <select id="default_lang">${(LANGS||[]).map(l=>html`<option value="${l.code}" ${l.code===(s.default_lang||"en")?"selected":""}>${l.name}</option>`)}</select></div>
    </div></div></div>
    <div class="card"><h3>Contacts</h3><div class="body">
      <div class="field" style="max-width:420px"><label>How visitors reach you</label>
        <select id="contact_mode">
          ${[["form","Contact form (messages land in the back-office)"],
             ["email","Email address, assembled in JavaScript against harvesters"],
             ["links","Links to your own tools"],
             ["off","Nothing at all"]].map(([v,l])=>
            html`<option value="${v}" ${(s.contact_mode||(s.contact_form?"form":"off"))===v?"selected":""}>${l}</option>`)}
        </select></div>
      <label class="chk"><input type="checkbox" id="captcha" ${s.captcha?"checked":""}>
        <span>Add a robot check to the form<small>A built-in proof-of-work check: nothing to read, so it
          works in every language, no third-party service, and no visitor data leaves your instance.
          Invisible to a person, expensive for a bot.</small></span></label>
      <div class="field"><label>Links (label | URL, one per line) — used by the “links” mode</label>
        <textarea id="contact_links" rows="3" placeholder="Support portal | https://support.example.net
Mastodon | https://mastodon.example/@noc">${(s.contact_links||[]).map(l=>`${l.label} | ${l.url}`).join("\n")}</textarea></div>
      <label class="chk"><input type="checkbox" id="show_email" ${s.show_email?"checked":""}>
        <span>Also publish the general email address<small>Off by default: an address on a public page
          ends up on spam lists. Leave it off to be reachable only through the form.</small></span></label>
      <div class="row2">
      ${f("contact_notify","Notify this address of new messages",s.contact_notify,"noc@example.net")}
      ${f("email","General email",s.email)}
      ${f("noc_email","NOC email",s.noc_email)}
      ${f("noc_phone","NOC phone",s.noc_phone)}
      ${f("url","Website",s.url)}
      ${f("peeringdb","PeeringDB record",s.peeringdb)}
      ${f("fed_url","Address of this instance",s.fed_url,"https://latency.example.net")}
      </div>
      <div class="note" style="margin:10px 0 0"><b>Address of this instance</b> is what a peer uses to
        reach it back, and pairing refuses to start without it. It is not the same as <b>Website</b>,
        which is your own site and may be a different domain. If <span class="mono">federation.base_url</span>
        is set in the configuration file, that value wins over this one.</div>
      <div class="note" style="margin:10px 0 0">The notification address is never shown publicly. It is
        used only if SMTP is configured in <a href="${pathOf("notify")}" data-goto="notify">NOC alerting</a>; otherwise
        messages simply wait in Messages.</div>
    </div></div>
    <div class="card"><h3>Public pages</h3><div class="body">
      <p class="note" style="margin-top:0">Untick a page to take it off the public site: it leaves the
        menu, the links of the other pages and the sitemap, and its address answers
        <span class="mono">404</span>. Federation stays out of the menu while federation is off,
        Pairing while federation is off or no AS number is set, and Host network until an AS number
        is set.</p>
      ${[["federation","Federation","Public peers, latency in both directions, the inter-AS matrix."],
         ["network","Host network","Your AS as global routing and PeeringDB see it."],
         ["pairing","Pairing","Identity, key fingerprint and how to request a pairing."]].map(([id,name,what])=>
        html`<label class="chk"><input type="checkbox" data-pubpage="${id}"
          ${(s.hidden_pages||[]).includes(id)?"":"checked"}><span>${name}<small>${what}</small></span></label>`)}
      <label class="chk"><input type="checkbox" checked disabled>
        <span>Home and About<small>Always published. About is where visitors find who runs this
          instance and how to reach you, and a public probe nobody can identify is not worth
          reading.</small></span></label>
      <div class="note" style="margin:10px 0 0"><b>Hiding a page is presentation, not policy.</b> The
        federation and pairing APIs other instances call keep answering, so hiding Federation or
        Pairing neither stops this instance federating nor breaks a pairing in progress. To stop
        federating, set <span class="mono">federation.enabled</span> to false in the configuration
        file.</div>
    </div></div>
    <div class="card"><h3>Search engines</h3><div class="body">
      <label class="chk"><input type="checkbox" id="search_index" ${s.search_index?"checked":""}>
        <span>Let search engines index the public pages<small>On by default. Each page gets a title, a
          description and a summary readable without JavaScript, every target gets a readable address
          (<span class="mono">/t/name</span>), and <span class="mono">/sitemap.xml</span> lists them all.
          Off puts a <span class="mono">noindex</span> on every page and blocks crawlers in
          <span class="mono">robots.txt</span>.</small></span></label>
    </div></div>
    <div class="card"><h3>Public traceroutes</h3><div class="body">
      <label class="chk"><input type="checkbox" id="public_traceroutes" ${s.public_traceroutes?"checked":""}>
        <span>Show traceroutes on the public target pages<small>Off by default: hops reveal the inside
          of your network (router addresses, reverse names). When on, visitors see the traceroutes
          taken during anomalies and the reference paths.</small></span></label>
    </div></div>
    <div class="card"><h3>Availability</h3><div class="body">
      <label class="chk"><input type="checkbox" id="public_availability" ${s.public_availability?"checked":""}>
        <span>Publish the availability figure on the target pages<small>On by default. It is the
          proportion of measurement passes where the target answered at least once, over 24 hours,
          7 days, 30 days and a year. It is not a loss rate and it is not an SLA, and the page says
          so twice next to it &mdash; but it is the one number on a public page a reader is likely to
          take for a commitment anyway. Off removes the block and stops
          <span class="mono">/api/v1/availability</span> answering public callers; the measurements
          are still taken and still counted.</small></span></label>
    </div></div>
    <div class="card"><h3>Addresses on public pages</h3><div class="body">
      <label class="chk"><input type="checkbox" id="mask_addresses" ${s.mask_addresses?"checked":""}>
        <span>Show only the network of an IP address<small>On by default. Masking reduces the
          exposure, it does not remove it: what a target is, and whether it should be published at
          all, stays the responsibility of whoever runs this instance. A visitor sees
          <span class="mono">142.251.XXX.XXX</span> instead of the address: the target's host when it
          is a literal address, the address actually probed under the route, and a pinned address.
          Host <em>names</em> are untouched — a name is not an address, and it is what says which
          service the page is about. You keep the full values everywhere once logged in.</small></span></label>
    </div></div>
    <div class="card"><h3>Default thresholds</h3><div class="body">
      <p class="note" style="margin-top:0">What separates <em>ok</em>, <em>warn</em> and
        <em>crit</em> for every target that does not set its own. A target known to be badly
        connected can accept more loss from its own settings, and one that matters can be watched
        more closely.</p>
      <div class="row2">
        <div class="field"><label>Warn above this loss (%)</label>
          <input id="th_lw" type="number" min="0.01" max="100" step="0.1"
                 value="${(s.thresholds&&s.thresholds.loss_warn)||0.4}"></div>
        <div class="field"><label>Critical above this loss (%)</label>
          <input id="th_lc" type="number" min="0.01" max="100" step="0.1"
                 value="${(s.thresholds&&s.thresholds.loss_crit)||3}"></div>
      </div>
      <div class="field"><label>Warn above this latency factor</label>
        <input id="th_lf" type="number" min="1.05" max="100" step="0.05"
               value="${(s.thresholds&&s.thresholds.lat_factor)||1.4}">
        <div style="font-size:11px;color:var(--ink3);margin-top:3px">Median compared with the
          seven-day baseline. 1.4 means "40 % slower than usual". A rise of less than a millisecond
          never raises anything, whatever the factor.</div></div>
    </div></div>
    <div class="card"><h3>Texts</h3><div class="body">
      <div class="field"><label>Description</label><textarea id="description" rows="3">${s.description||""}</textarea></div>
      <div class="field"><label>Legal notice</label><textarea id="legal" rows="2">${s.legal||""}</textarea></div>
      <button class="btn p" id="save">Save</button>
    </div></div>`);
    document.querySelectorAll("[data-goto]").forEach(a=>a.onclick=e=>{e.preventDefault();go(a.dataset.goto);});
    $("#save").onclick=async()=>{
      const ids=["title","org","asn","owner","location","timezone","email",
                 "noc_email","noc_phone","url","fed_url","peeringdb","description","legal","default_lang",
                 "contact_notify"];
      const body={};ids.forEach(k=>body[k]=$("#"+k).value);
      body.public_traceroutes=$("#public_traceroutes").checked;
      body.contact_mode=$("#contact_mode").value;
      body.contact_form=$("#contact_mode").value==="form";
      body.captcha=$("#captcha").checked;
      body.contact_links=$("#contact_links").value.split("\n").map(l=>{
        const i=l.indexOf("|"); if(i<0) return null;
        const label=l.slice(0,i).trim(), url=l.slice(i+1).trim();
        return (label&&url)?{label,url}:null;}).filter(Boolean);
      body.search_index=$("#search_index").checked;
      body.hidden_pages=Array.from(document.querySelectorAll("[data-pubpage]"))
        .filter(c=>!c.checked).map(c=>c.dataset.pubpage);
      body.public_availability=$("#public_availability").checked;
      body.mask_addresses=$("#mask_addresses").checked;
      body.thresholds={loss_warn:parseFloat($("#th_lw").value||"0"),
                       loss_crit:parseFloat($("#th_lc").value||"0"),
                       lat_factor:parseFloat($("#th_lf").value||"0")};
      body.show_email=$("#show_email").checked;
      try{await api("PUT","/api/v1/admin/site",body);toast("Saved");}
      catch(e){toast(e.message,true);}
    };
  }catch(e){H.render($("#s"),html`<div class="note">${e.message}</div>`);}
}
