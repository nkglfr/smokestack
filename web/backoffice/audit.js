/* smokestack back-office — share links, service log and audit log.
 * Charge par admin.html, dans l'ordre : ces fichiers partagent une
 * portee globale, comme app.js et i18n.js du site public. */
/* --------------------------------------------------------- audit log */
async function viewShares(m){
  H.render(m,html`<h2>Share links</h2>
    <p class="lead"><strong>What this is for.</strong> Troubleshooting across networks. When you open a
      ticket with a transit provider or write to another AS's NOC, the hard part is not describing the
      problem: it is proving it. A share link hands them your own measurement — a year of percentiles
      towards one destination, the loss, and the traceroutes taken exactly when the path degraded —
      without an account, without a screenshot they cannot verify, and without access to the rest of
      your monitoring. The other side sees where it breaks from your point of view and can compare with
      their own view of the same path. The same link works for a customer asking about their link, or
      for a peer investigating an asymmetry with you.</p>
    <p class="lead">A link opens that one target and nothing else, even a private one, is never indexed,
      expires on the date you chose, and can be revoked here — access stops immediately. The token is
      stored hashed: a copy of the database hands over no working link, and the link itself is shown
      once, when created, from the target list.</p>
    <div class="card"><div class="body" id="shl">Loading…</div></div>`);
  try{
    const L=await api("GET","/api/v1/admin/shares");
    H.render($("#shl"),(L&&L.length)?html`<table class="resp"><thead><tr><th>Target</th><th>Note</th>
      <th>Created</th><th>Expires</th><th>Used</th><th></th></tr></thead><tbody>${L.map(l=>html`<tr>
      <td class="ttl" data-l=""><div>${l.title||("#"+l.target_id)}</div>
        <div style="font-size:11.5px;color:var(--ink3)">by ${l.created_by||"—"}</div></td>
      <td data-l="Note">${l.note||"—"}</td>
      <td data-l="Created">${new Date(l.created_at*1000).toLocaleDateString()}</td>
      <td data-l="Expires">${l.expires_at?
        (l.expires_at*1000<Date.now()?html`<span class="badge b-crit">expired</span>`:new Date(l.expires_at*1000).toLocaleDateString())
        :html`<span class="badge b-warn">never</span>`}</td>
      <td data-l="Used">${l.uses||0}${l.last_used?" · "+new Date(l.last_used*1000).toLocaleDateString():""}</td>
      <td class="acts" data-l=""><button class="btn d s" data-rev="${l.id}">Revoke</button></td>
      </tr>`)}</tbody></table>`:html`<div class="empty">No share link. Create one from the target list.</div>`);
    document.querySelectorAll("[data-rev]").forEach(b=>b.onclick=async()=>{
      if(!confirm("Revoke this link? Whoever has it loses access immediately.")) return;
      try{await api("DELETE","/api/v1/admin/shares/"+b.dataset.rev);toast("Link revoked");render();}
      catch(e){toast(e.message,true);}
    });
  }catch(e){H.render($("#shl"),html`<div class="note">${e.message}</div>`);}
}

async function viewLogs(m){
  H.render(m,html`<h2>Service log</h2>
    <p class="lead">The last lines the service wrote, kept in memory. The same lines go to
      <span class="mono">journalctl -u smokestack</span> or <span class="mono">docker compose logs</span>,
      which hold the full history; this screen is for when you do not have a shell at hand.
      One line appears when a target starts failing, with the reason, and one when it answers again.</p>
    <div class="acts">
      <input id="lgf" placeholder="filter, e.g. target or update" style="max-width:280px">
      <label class="chk" style="margin:0"><input type="checkbox" id="lgauto"><span>Refresh every 5 s</span></label>
      <button class="btn" id="lgr">Refresh</button></div>
    <div class="card"><div class="body" style="padding:0"><pre id="lg" style="margin:0;padding:12px;
      max-height:65vh;overflow:auto;font-size:12px;line-height:1.5;white-space:pre-wrap">Loading…</pre></div></div>
    <div id="lgnote"></div>`);
  let TIMER=null;
  const load=async()=>{
    try{
      const d=await api("GET","/api/v1/admin/logs");
      const f=$("#lgf").value.trim().toLowerCase();
      const lines=(d.lines||[]).filter(l=>!f||l.toLowerCase().includes(f));
      $("#lg").textContent=lines.length?lines.join("\n"):"(nothing yet)";
      $("#lg").scrollTop=$("#lg").scrollHeight;
      H.render($("#lgnote"),d.probe?"":html`<div class="note">The probe runs in its own process: its own
        lines are in <span class="mono">journalctl -u smokestack-probe</span> or in the container log,
        not here.</div>`);
    }catch(e){$("#lg").textContent=e.message;}
  };
  $("#lgr").onclick=load; $("#lgf").oninput=load;
  $("#lgauto").onchange=()=>{ if($("#lgauto").checked){TIMER=setInterval(load,5000);} else {clearInterval(TIMER);} };
  load();
}

async function viewAudit(m){
  H.render(m,html`<h2>Audit log</h2>
    <p class="lead">The last two hundred administrative actions, including sign-ins and failed sign-ins.</p>
    <div class="card"><div class="body" id="l">Loading…</div></div>`);
  try{
    const list=await api("GET","/api/v1/admin/audit");
    H.render($("#l"),(list&&list.length)?html`<table><thead><tr><th>Date</th><th>Account</th>
      <th>Action</th><th>Object</th><th>IP</th></tr></thead><tbody>${list.map(e=>html`<tr><td style="font-size:12px">${dt(e.ts)}</td>
        <td style="font-size:12px">${e.email||"—"}${e.key_name?html`<div style="color:var(--ink3)">with key ${e.key_name}</div>`:""}</td>
        <td><span class="badge ${e.action==="login_failed"?"b-crit":"b-n"}">${e.action}</span></td>
        <td style="font-size:12px">${e.entity} ${e.entity_id}</td>
        <td class="mono" style="font-size:12px">${e.ip}</td></tr>`)}</tbody></table>`:html`<div class="empty">The log is empty.</div>`);
  }catch(e){H.render($("#l"),html`<div class="note">${e.message}</div>`);}
}
