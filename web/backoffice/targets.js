/* smokestack back-office — targets and categories.
 * Charge par admin.html, dans l'ordre : ces fichiers partagent une
 * portee globale, comme app.js et i18n.js du site public. */
/* ----------------------------------------------------------- targets */
const FAMILY={0:"auto",4:"IPv4",6:"IPv6"};
const COMMON_PORTS=[[443,"HTTPS"],[80,"HTTP"],[22,"SSH"],[53,"DNS"],[25,"SMTP"],[587,"SMTP submission"],[179,"BGP"],[3306,"MySQL"],[5432,"PostgreSQL"],[8080,"HTTP alt"]];
let EDIT=null;   // target being edited, null when creating

async function viewTargets(m){
  const editing=!!EDIT;
  H.render(m,html`<h2>Targets and categories</h2>
    <p class="lead">A burst must fit in its interval: packets × spacing + timeout must stay
      below 75 % of the interval. At 30 s, use 10 packets spaced by 200 ms.
      A <strong>private</strong> target is measured like any other but never appears on the public
      site: useful for tests, for internal addresses, or for a customer link.</p>

    <div class="card"><h3>Categories</h3><div class="body">
      <p style="margin:0 0 8px;font-size:13px;color:var(--ink2)">The order here is the order of the
        sections on the public page.</p>
      <div id="clist">Loading…</div>
      <div class="row2" style="margin-top:14px">
        <div class="field"><label>Short identifier</label><input id="cslug" placeholder="transit"></div>
        <div class="field"><label>English label</label><input id="cen" placeholder="Transit providers"></div>
        <div class="field"><label>French label</label><input id="cfr" placeholder="Transitaires"></div>
      </div>
      <button class="btn p" id="addcat">Create the category</button>
    </div></div>

    <div class="card" ${editing?'style="border-color:#93c5fd"':''}><h3>${editing?`Edit “${EDIT.title}”`:"New target"}</h3><div class="body">
      <div class="row2">
        <div class="field"><label>Category</label><select id="tcat"></select></div>
        <div class="field"><label>Title</label><input id="ttitle" placeholder="Cogent Paris"></div>
        <div class="field" id="tslugfield" hidden><label>Public address</label>
          <input id="tslug" class="mono" placeholder="cogent-paris">
          <div style="font-size:11px;color:var(--ink3);margin-top:3px">The page is served at
            <span class="mono">/t/…</span> under this. Renaming a target leaves it alone, so a link
            already in somebody's ticket keeps working; change it here when you want the old name
            free for another target. Existing share links follow the target, not the address.</div></div>
        <div class="field"><label>Host or IP address</label><input id="thost" placeholder="192.0.2.1 or 2001:db8::1"></div>
        <div class="field"><label>Protocol</label><select id="tproto">
          <option value="icmp">ICMP</option><option value="tcp">TCP connect</option>
          <option value="udp">UDP echo</option></select></div>
        <div class="field" id="portfield" hidden><label id="portlabel">TCP port</label>
          <input id="tport" list="ports" placeholder="443">
          <datalist id="ports">${COMMON_PORTS.map(([p,l])=>html`<option value="${p}">${p} — ${l}</option>`)}</datalist>
          <div id="portnote" style="font-size:11px;color:var(--ink3);margin-top:3px" hidden>Port 7 is the RFC 862 echo port, which a Junos probe-server listens on. A receiver can be told to use the 49160–65535 range instead.</div></div>
        <div class="note" style="grid-column:1/-1">Prefer a name over a public IP address.
          Publishing the address of a machine you do not own exposes it, and that stays the
          responsibility of whoever runs this instance — not of the tool. Public pages show only
          the network of an address, but the safest address is the one that never leaves the
          back-office: use <em>hide the address on the public page</em> when it must not appear
          at all.</div>
        <div class="field"><label>Address family</label><select id="tfam">
          <option value="4">IPv4</option><option value="6">IPv6</option>
          <option value="0" id="tfamauto" hidden>Auto (legacy: IPv4 if there is one, else IPv6)</option></select>
          <div style="font-size:11px;color:var(--ink3);margin-top:3px">A target states which family it
            measures. The two cross different networks and fail independently, so measure both with
            <em>Also IPv6</em>, which keeps them as two comparable series.</div></div>
        <div class="field"><label>Interval (seconds, 10 to 86400 — empty inherits from the category)</label>
          <input id="tiv" type="number" min="10" max="86400" placeholder="—" list="ivs">
          <datalist id="ivs">${[[30,"30 s"],[60,"1 min"],[120,"2 min"],[300,"5 min"],[600,"10 min"],
            [1800,"30 min"],[3600,"1 hour"]].map(([v,l])=>html`<option value="${v}">${l}</option>`)}</datalist>
          <div style="font-size:11px;color:var(--ink3);margin-top:3px">The burst must fit:
            packets × spacing + timeout under 75 % of the interval.</div></div>
        <div class="field"><label>Keep measurements for (days, 0 = instance tiers)</label>
          <input id="tkeep" type="number" min="0" max="3650" value="0"></div>
        <div class="field"><label>Warn above this loss (%, 0 = instance default)</label>
          <input id="tlw" type="number" min="0" max="100" step="0.1" value="0"></div>
        <div class="field"><label>Critical above this loss (%, 0 = instance default)</label>
          <input id="tlc" type="number" min="0" max="100" step="0.1" value="0"></div>
        <div class="field"><label>Warn above this latency factor (0 = instance default)</label>
          <input id="tlf" type="number" min="0" max="100" step="0.05" value="0">
          <div style="font-size:11px;color:var(--ink3);margin-top:3px">Median compared with the
            seven-day baseline. 1.4 means "40 % slower than usual". A rise of less than a
            millisecond never raises anything, whatever the factor.</div></div>
        <div class="field"><label>Packets <span class="faint" style="font-weight:400">— empty inherits</span></label>
          <input id="tpk" type="number" placeholder="—"></div>
        <div class="field"><label>Spacing (ms) <span class="faint" style="font-weight:400">— empty inherits</span></label>
          <input id="tsp" type="number" placeholder="—"></div>
        <div class="field"><label>Timeout (ms) <span class="faint" style="font-weight:400">— empty inherits</span></label>
          <input id="tto" type="number" placeholder="—"></div>
        <div class="note" id="tinh" style="grid-column:1/-1;display:none"></div>
        <div class="field"><label>Reference traceroute every (hours, 0 = instance default)</label>
          <input id="thrs" type="number" min="0" max="720" value="0"></div>
        <div class="field"><label>Pinned address (optional)</label>
          <input id="tpin" placeholder="leave empty to resolve the name each time"></div>
        <div class="field"><label>Measured from</label>
          <select id="tprim" disabled><option value="0">This instance</option></select>
          <div id="tprimnote" style="font-size:11px;color:var(--ink3);margin-top:3px"></div>
          <div style="font-size:11px;color:var(--ink3);margin-top:3px">The probe chosen here decides this
            target's status and runs its traceroutes; the others, if any, still measure it.</div></div>
      </div>
      <label class="chk"><input type="checkbox" id="thide"><span>Hide the address on the public page
        <small>The graph stays public, the host or IP is not shown, and neither are its traceroutes.
          For a dashboard you give to customers without disclosing your addressing.</small></span></label>
      <label class="chk"><input type="checkbox" id="talert" checked><span>Alert on this target
        <small>When it stays in incident longer than the threshold set in <em>My targets alerting</em>.
          Uncheck for a target you watch but never want to be woken up for.</small></span></label>
      <label class="chk"><input type="checkbox" id="tpub" checked><span>Public target
        <small>Shown on the public site. Uncheck for a private target: it is measured and visible
          here, but never appears on the public pages or in the public API.</small></span></label>
      <div class="acts">
        <button class="btn p" id="savetgt">${editing?"Save changes":"Create the target"}</button>
        ${editing?html`<button class="btn" id="canceledit">Cancel</button>`:""}
      </div>
      ${editing?"":html`<div class="note" style="margin:12px 0 0">A new target is measured right away:
        its first result appears within a few seconds, without waiting a whole interval.</div>`}
    </div></div>
    <div class="card"><h3>Ready-made targets</h3><div class="body">
      <p style="margin:0 0 10px;font-size:13px;color:var(--ink2)">Well-known public services, in their
        IPv4 and IPv6 form, with gentle settings (10 packets spaced by 200 ms). Tick what you want;
        their category is created if needed, and they are measured right away.<br>
        Looking for targets in a given country or region? The
        <a href="https://github.com/nkglfr/smokestack/wiki" target="_blank" rel="noreferrer">project wiki</a>
        keeps pages of suggested targets per country, kept up to date by users, and a page on
        <a href="https://github.com/nkglfr/smokestack/wiki/Configuring-targets" target="_blank" rel="noreferrer">sizing
        and adjusting targets</a> (rate limiting, rotating names, suggested settings).</p>
      <div id="sug">Loading…</div></div></div>
    <div class="card"><h3>Targets</h3><div class="body" id="tlist">Loading…</div></div>
    <div class="card"><h3>Archived targets</h3><div class="body">
      <p style="margin:0 0 10px;font-size:13px;color:var(--ink2)">Deleting a target archives it: its
        measurements stay, its name becomes free again, and a new target of the same name never inherits
        its history. Purging removes an archived target and its measurements for good.</p>
      <div id="arch">Loading…</div></div></div>`);

  const tree=await api("GET","/api/v1/tree").catch(()=>[]);
  H.render($("#tcat"),(tree||[]).map(c=>
    html`<option value="${c.id}">${c.menu_en||c.menu_fr}</option>`)||
    html`<option value="">— create a category first —</option>`);

  /* categories */
  H.render($("#clist"),(tree&&tree.length)?html`<table><thead><tr><th>English label</th><th>French label</th>
    <th>Targets</th><th></th></tr></thead><tbody>${tree.map(c=>html`<tr>
      <td><input data-cen="${c.id}" value="${c.menu_en||""}" style="padding:4px 8px"></td>
      <td><input data-cfr="${c.id}" value="${c.menu_fr||""}" style="padding:4px 8px"></td>
      <td>${(c.targets||[]).length}</td>
      <td style="text-align:right;white-space:nowrap">
        <button class="btn s" data-cup="${c.id}" title="Move up">↑</button>
        <button class="btn s" data-cdown="${c.id}" title="Move down">↓</button>
        <button class="btn s" data-csave="${c.id}">Rename</button>
        <button class="btn s" data-cpar="${c.id}">Parameters</button>
        <button class="btn d s" data-cdel="${c.id}">Delete</button></td></tr>
      <tr id="cpar${c.id}" hidden><td colspan="4" style="background:var(--bg2)">
        <p class="note" style="margin-top:0">A value here applies to every target of this category
          that leaves the field empty. Changing it changes what those targets measure, at once —
          the count next to each field is how many that is right now. Empty means the category lends
          nothing and the target keeps what it has.</p>
        <div class="row2">${[["interval_s","Interval (s)",10,86400,1],
                             ["packets","Packets",3,50,1],
                             ["spacing_ms","Spacing (ms)",0,60000,1],
                             ["timeout_ms","Timeout (ms)",0,60000,1],
                             ["keep_days","Keep (days)",0,3650,1],
                             ["trace_hours","Reference traceroute (h)",0,720,1],
                             ["loss_warn","Warn above loss (%)",0,100,0.1],
                             ["loss_crit","Critical above loss (%)",0,100,0.1],
                             ["lat_factor","Latency factor",0,100,0.05]]
          .map(([k,label,min,max,step])=>html`<div class="field"><label>${label}
            <span class="faint" style="font-weight:400" id="cnt-${c.id}-${k}"></span></label>
            <input id="cp-${c.id}-${k}" type="number" min="${min}" max="${max}" step="${step}"
              value="${(c.params&&c.params[k])||""}" placeholder="—"></div>`)}</div>
        <div class="acts">
          <button class="btn p s" data-cpsave="${c.id}">Save parameters</button>
          <button class="btn s" data-cpreset="${c.id}"
            title="Clear these fields on every target of the category, so its values apply">
            Make its targets inherit…</button></div></td></tr>`)}</tbody></table>`:html`<div class="empty">No category yet.</div>`);
  document.querySelectorAll("[data-csave]").forEach(b=>b.onclick=async()=>{
    const id=b.dataset.csave;
    try{await api("PATCH","/api/v1/admin/categories/"+id,
      {menu_en:document.querySelector(`[data-cen="${id}"]`).value,
       menu_fr:document.querySelector(`[data-cfr="${id}"]`).value});
      toast("Category renamed");render();}catch(e){toast(e.message,true);}
  });
  document.querySelectorAll("[data-cpar]").forEach(b=>b.onclick=async()=>{
    const id=b.dataset.cpar, row=document.getElementById("cpar"+id);
    row.hidden=!row.hidden;
    if(row.hidden) return;
    // How many targets take each value from the category, so the edit states
    // its own reach before it is saved.
    try{const r=await api("GET","/api/v1/admin/categories/"+id+"/params");
      Object.entries(r.inheriting||{}).forEach(([k,n])=>{
        const el=document.getElementById("cnt-"+id+"-"+k);
        if(el) el.textContent=n?`· ${n} target${n>1?"s":""} inherit${n>1?"":"s"}`:"· none inherits";
      });}catch(e){/* the fields still work without the counts */}
  });
  document.querySelectorAll("[data-cpsave]").forEach(b=>b.onclick=async()=>{
    const id=b.dataset.cpsave, body={};
    ["interval_s","packets","spacing_ms","timeout_ms","keep_days","trace_hours",
     "loss_warn","loss_crit","lat_factor"].forEach(k=>{
      const v=(document.getElementById("cp-"+id+"-"+k).value||"").trim();
      body[k]=v===""?0:parseFloat(v);
    });
    try{const r=await api("PUT","/api/v1/admin/categories/"+id+"/params",body);
      toast("Parameters saved");render();}catch(e){toast(e.message,true);}
  });
  document.querySelectorAll("[data-cpreset]").forEach(b=>b.onclick=async()=>{
    const id=b.dataset.cpreset;
    const fields=["interval_s","packets","spacing_ms","timeout_ms","keep_days","trace_hours",
      "loss_warn","loss_crit","lat_factor"].filter(k=>
        (document.getElementById("cp-"+id+"-"+k).value||"").trim()!=="");
    if(!fields.length){toast("Set a value on this category first",true);return;}
    if(!confirm("Clear "+fields.join(", ")+" on every target of this category, so the category's "+
      "values apply to all of them? Measurements are untouched; only what the next one does changes."))
      return;
    try{const r=await api("POST","/api/v1/admin/categories/"+id+"/reset",{fields});
      toast(r.targets+" target(s) now inherit");render();}catch(e){toast(e.message,true);}
  });
  const move=async(id,dir)=>{
    try{await api("POST","/api/v1/admin/categories/"+id+"/move?dir="+dir);render();}
    catch(e){toast(e.message,true);}
  };
  document.querySelectorAll("[data-cup]").forEach(b=>b.onclick=()=>move(b.dataset.cup,"up"));
  document.querySelectorAll("[data-cdown]").forEach(b=>b.onclick=()=>move(b.dataset.cdown,"down"));
  document.querySelectorAll("[data-cdel]").forEach(b=>b.onclick=async()=>{
    if(!confirm("Delete this category?")) return;
    try{await api("DELETE","/api/v1/admin/categories/"+b.dataset.cdel);toast("Category deleted");render();}
    catch(e){toast(e.message,true);}
  });
  $("#addcat").onclick=async()=>{
    try{
      await api("POST","/api/v1/admin/categories",
        {slug:$("#cslug").value,menu_en:$("#cen").value,menu_fr:$("#cfr").value||$("#cen").value});
      toast("Category created");render();
    }catch(e){toast(e.message,true);}
  };

  /* target form */
  // Clean a pasted value straight away: a leading tab, a trailing space, a
  // whole URL. The server checks it again anyway.
  const cleanHost=v=>v.replace(/[\u00a0\u200b-\u200d\ufeff]/g,"").trim()
    .replace(/^[a-z][a-z0-9+.-]*:\/\//i,"").replace(/[/?#].*$/,"")
    .replace(/^\[|\]$/g,"").replace(/\.$/,"");
  $("#thost").addEventListener("blur",()=>{$("#thost").value=cleanHost($("#thost").value);});
  $("#thost").addEventListener("paste",e=>{
    const txt=(e.clipboardData||window.clipboardData).getData("text");
    if(txt){e.preventDefault();$("#thost").value=cleanHost(txt);}
  });
  // A UDP echo target needs a port as much as a TCP one does. Leaving this on "tcp" alone was the
  // one-line gap that would have shipped a protocol with no way to set the port it measures.
  const syncProto=()=>{
    const p=$("#tproto").value, needsPort=p==="tcp"||p==="udp";
    $("#portfield").hidden=!needsPort;
    $("#portlabel").textContent=p==="udp"?"UDP port":"TCP port";
    $("#portnote").hidden=p!=="udp";
    if(p==="udp"&&!$("#tport").value) $("#tport").value="7";
  };
  $("#tproto").onchange=syncProto;
  if(EDIT){
    $("#tcat").value=EDIT.category_id; $("#ttitle").value=EDIT.title; $("#thost").value=EDIT.host;
    $("#tproto").value=EDIT.proto; $("#tport").value=EDIT.port||""; $("#tfam").value=String(EDIT.family||0);
    $("#tlw").value=EDIT.loss_warn||0; $("#tlc").value=EDIT.loss_crit||0; $("#tlf").value=EDIT.lat_factor||0;
    // Auto is no longer offered for a new target; a target that already has
    // it keeps it visible, so editing something else does not silently
    // change what it measures.
    if((EDIT.family||0)===0){ $("#tfamauto").hidden=false; $("#tfam").value="0"; }
    // The public address is only worth showing for a target that has one:
    // at creation it is derived from the title, and made free if taken.
    $("#tslugfield").hidden=false; $("#tslug").value=EDIT.slug||"";
    // Zero is stored for a field that inherits, and the form shows it empty:
    // the difference between "inherits 60 s" and "states 60 s" is the whole
    // point, and prefilling would quietly turn the first into the second.
    const z=v=>(v?String(v):"");
    $("#tiv").value=z(EDIT.interval_s); $("#tkeep").value=EDIT.keep_days||0;
    $("#tpk").value=z(EDIT.packets); $("#tsp").value=z(EDIT.spacing_ms);
    $("#tto").value=z(EDIT.timeout_ms); $("#tpub").checked=!!EDIT.public;
    $("#canceledit").onclick=()=>{EDIT=null;render();};
  }
  // A new target inherits by default, which is what makes a category
  // parameter worth setting.
  if(!EDIT){ $("#tslugfield").hidden=true; $("#tslug").value=""; }
  showInherited($("#tcat").value);
  $("#tcat").onchange=()=>showInherited($("#tcat").value);
  syncProto();
  // "Measured from": only enabled remote probes that are assigned this target are offered, because the
  // server refuses any other. It is sent only once the operator changes it, so saving an unrelated edit
  // never rewrites a choice made elsewhere in the meantime.
  let primaryTouched=false;
  $("#tprim").onchange=()=>{primaryTouched=true;};
  if(!EDIT){
    H.render($("#tprimnote"),html`Save the target first, then assign it to a probe on the Probes page.`);
  }else{
    const editId=EDIT.id, EDIT_PRIMARY=EDIT.primary_probe_id||0;
    try{
      const remote=((await api("GET","/api/v1/admin/probes")).probes||[]).filter(p=>p.enabled&&!p.local);
      const assigned=await Promise.all(remote.map(async p=>
        ((await api("GET","/api/v1/admin/probes/"+p.id+"/targets")).targets||[]).includes(editId)));
      const offered=remote.filter((p,i)=>assigned[i]);
      const cur=EDIT_PRIMARY;
      // A primary that is no longer among the offered probes means it was unassigned from this target
      // after being chosen. Showing it rather than silently falling back to "This instance" is what tells
      // the operator their choice is no longer being honoured.
      const stale=cur!==0&&!offered.some(p=>p.id===cur);
      if(offered.length||stale){
        H.render($("#tprim"),html`<option value="0">This instance</option>${offered.map(p=>
          html`<option value="${p.id}">${p.name||p.slug}</option>`)}${stale?
          html`<option value="${cur}">probe ${cur} — no longer assigned this target</option>`:""}`);
        $("#tprim").value=String(cur);
        $("#tprim").disabled=false;
        if(stale) H.render($("#tprimnote"),html`The chosen probe is no longer assigned this target: assign it again on the Probes page, or pick another.`);
      }else{
        H.render($("#tprimnote"),html`Unavailable: this target must first be assigned to a probe on the Probes page.`);
      }
    }catch(e){
      H.render($("#tprimnote"),html`Unavailable: the probes could not be read (${e.message}).`);
    }
  }
  $("#savetgt").onclick=async()=>{
    const primaryId=(primaryTouched&&!$("#tprim").disabled)?parseInt($("#tprim").value,10):null;
    const body={category_id:parseInt($("#tcat").value,10),title:$("#ttitle").value,
      host:cleanHost($("#thost").value),family:parseInt($("#tfam").value,10),proto:$("#tproto").value,
      port:($("#tproto").value==="tcp"||$("#tproto").value==="udp")?parseInt($("#tport").value||"0",10):0,
      interval_s:num0($("#tiv").value), keep_days:parseInt($("#tkeep").value||"0",10),
      packets:num0($("#tpk").value), spacing_ms:num0($("#tsp").value), timeout_ms:num0($("#tto").value),
      pin_ip:$("#tpin").value.trim(), alerts_off:!$("#talert").checked,
      trace_hours:parseInt($("#thrs").value||"0",10), hide_host:$("#thide").checked,
      loss_warn:parseFloat($("#tlw").value||"0"), loss_crit:parseFloat($("#tlc").value||"0"),
      lat_factor:parseFloat($("#tlf").value||"0"),
      public:$("#tpub").checked};
    try{
      if(EDIT){
        const sl=($("#tslug").value||"").trim();
        if(sl && sl!==EDIT.slug) body.slug=sl;
        await api("PATCH","/api/v1/admin/targets/"+EDIT.id,body);
        const savedId=EDIT.id; EDIT=null; toast("Target saved");
        // Its own request, right after the target is saved: a refusal must not hide that the target was saved.
        if(primaryId!==null){
          try{await api("PUT","/api/v1/admin/targets/"+savedId+"/primary",{probe_id:primaryId});}
          catch(e){toast("Target saved, but the probe was not changed: "+e.message,true);}
        } }
      else { await api("POST","/api/v1/admin/targets",Object.assign(body,{enabled:true})); toast("Target created, first measurement under way"); }
      render();
    }catch(e){toast(e.message,true);}
  };

  /* ready-made targets */
  try{
    const sug=await api("GET","/api/v1/admin/suggested");
    const groups={};
    (sug||[]).forEach(x=>{(groups[x.group]=groups[x.group]||[]).push(x);});
    H.render($("#sug"),Object.entries(groups).map(([g,items])=>html`
      <div style="margin-bottom:10px"><div style="font-weight:600;font-size:12.5px;margin-bottom:4px">${g}</div>
      ${items.map(x=>html`<label class="chk" style="margin:3px 0">
        <input type="checkbox" data-sug="${x.key}" ${x.existing?"disabled":""}>
        <span>${x.title} <span class="mono" style="color:var(--ink2)">${x.host}${x.port?":"+x.port:""}</span>
          ${x.existing?html`<span class="badge b-n">already added</span>`:""}
          ${x.note?html`<small>${x.note}</small>`:""}</span></label>`)}</div>`).concat(
      html`<button class="btn p" id="addsug">Add the selected targets</button>`));
    $("#addsug").onclick=async()=>{
      const keys=[...document.querySelectorAll("[data-sug]:checked")].map(i=>i.dataset.sug);
      if(!keys.length){toast("Nothing selected",true);return;}
      try{const r=await api("POST","/api/v1/admin/suggested",{keys});
        toast(`${r.added} target(s) added`+(r.skipped?`, ${r.skipped} skipped`:""));render();}
      catch(e){toast(e.message,true);}
    };
  }catch(e){H.render($("#sug"),html`<div class="note">${e.message}</div>`);}

  /* archived targets */
  try{
    const arch=await api("GET","/api/v1/admin/targets/archived");
    H.render($("#arch"),(arch&&arch.length)?html`<table class="resp"><thead><tr><th>Target</th>
      <th>Archived</th><th></th></tr></thead><tbody>${arch.map(t=>html`<tr>
      <td class="ttl" data-l=""><div>${t.title}</div>
        <div class="mono" style="font-size:11.5px;color:var(--ink2)">${t.host}${t.port?":"+t.port:""}</div></td>
      <td data-l="Archived">${new Date(t.archived_at*1000).toLocaleDateString()}</td>
      <td class="acts" data-l=""><button class="btn d s" data-purge="${t.id}">Purge for good</button></td>
      </tr>`)}</tbody></table>`:html`<div class="empty">Nothing archived.</div>`);
    document.querySelectorAll("[data-purge]").forEach(b=>b.onclick=async()=>{
      if(!confirm("Purge this archived target and all its measurements? This cannot be undone.")) return;
      try{await api("DELETE","/api/v1/admin/targets/"+b.dataset.purge+"?purge=1");
        toast("Archived target purged");render();}catch(e){toast(e.message,true);}
    });
  }catch(e){H.render($("#arch"),html`<div class="note">${e.message}</div>`);}

  /* target list */
  try{
    const [list,feat]=await Promise.all([api("GET","/api/v1/admin/targets"),api("GET","/api/v1/admin/featured").catch(()=>[])]);
    const F=new Set(feat||[]);
    // Host and category sit under the title: fewer columns, and the table
    // turns into one card per target on a phone.
    const cname={}; (tree||[]).forEach(c=>cname[c.id]=c.menu_en||c.menu_fr);
    H.render($("#tlist"),(list&&list.length)?html`<table class="resp">
      <thead><tr><th title="Critical target: always shown at the top of the home page">★</th><th>Target</th>
      <th>Family</th><th>Check</th><th>Interval</th><th>Alerts</th><th>Visibility</th><th></th></tr></thead><tbody>${list.map(t=>html`<tr><td data-l=""><button class="btn s" data-star="${t.id}" title="Critical target"
          style="color:${F.has(t.id)?"#d97706":"var(--ink3)"}">${F.has(t.id)?"★":"☆"}</button></td>
        <td class="ttl" data-l="" style="text-align:left"><div style="font-weight:550">${t.title}</div>
          <div class="mono" style="font-size:11.5px;color:var(--ink2)">${t.host}${t.port?":"+t.port:""}</div>
          <div style="font-size:11.5px;color:var(--ink3)">${cname[t.category_id]||"—"}</div>
          ${t.hide_host?html`<div class="tpin">🙈 address hidden on the public page</div>`:""}
          ${t.pin_ip?html`<div class="tpin">📌 pinned to ${t.pin_ip}
            <button class="btn s" data-unpin="${t.id}">Unpin</button></div>`:
            ((t.addresses||[]).length>1?html`<div class="twarn">This name answered from
              ${(t.addresses||[]).length} different addresses in the last 24 h
              (${(t.addresses||[]).slice(0,3).join(", ")}${(t.addresses||[]).length>3?"…":""}).
              The graph mixes several machines, and a server that does not answer ICMP shows up as loss.
              <button class="btn s" data-pin="${t.id}" data-ip="${t.addresses[0]}">Pin ${t.addresses[0]}</button>
              </div>`:"")}
          ${(t.family||0)===0?html`<div class="${t.mixed_family?"twarn":"tpin"}">
            ${t.mixed_family
              ? html`This target states no address family and its measurements <strong>mixed IPv4 and
                 IPv6</strong> in the last 24 h. The two cross different networks, so its history
                 holds two paths with no way to tell them apart. Pick one:`
              : html`This target states no address family. It measured over
                 <strong>IPv${t.measured_family||4}</strong>, but that can change on its own if the
                 name gains or loses a record. State it:`}
            <button class="btn s" data-fam4="${t.id}">Set IPv4</button>
            <button class="btn s" data-fam6="${t.id}">Set IPv6</button></div>`:""}
          ${t.last_error?html`<div class="terr" title="Last failure: ${new Date(t.last_error_ts*1000).toLocaleString()}">
            ⚠ ${t.last_error}</div>`:""}</td>
        <td data-l="Family">${FAMILY[t.family||0]}</td><td data-l="Check">${t.proto}</td>
        <td data-l="Interval">${t.interval_s} s</td>
        <td data-l="Alerts"><button class="btn s" data-alert="${t.id}" data-off="${t.alerts_off?1:0}"
          title="Alert when this target stays in incident">${t.alerts_off?
            html`<span class="badge b-n">off</span>`:html`<span class="badge b-ok">on</span>`}</button></td>
        <td data-l="Visibility"><button class="btn s" data-vis="${t.id}" data-pub="${t.public?1:0}"
          title="Click to switch">${t.public?html`<span class="badge b-ok">public</span>`:html`<span class="badge b-warn">private</span>`}</button></td>
        <td class="acts" data-l="" style="text-align:right;white-space:nowrap">
          <button class="btn s" data-edit="${t.id}">Edit</button>
          <button class="btn s" data-now="${t.id}" title="Measure right away">Check now</button>
          <button class="btn s" data-share="${t.id}">Share</button>
          <button class="btn s" data-advice="${t.id}" title="Look at the shape of the loss">Why this loss?</button>
          <button class="btn s" data-trace="${t.id}">Traceroute</button>
          <button class="btn s" data-twin="${t.id}" data-fam="${t.family||0}"
            title="Measure the same service in the other address family, as its own target">
            ${t.family===4?"Also IPv6":t.family===6?"Also IPv4":"Split v4 / v6"}</button>
          <button class="btn d s" data-del="${t.id}">Delete</button></td>
        </tr>`)}</tbody></table>`:html`<div class="empty">No target yet.</div>`);
    const setFamily=async(id,fam)=>{
      try{await api("PATCH","/api/v1/admin/targets/"+id,{family:fam});
        toast("Target set to IPv"+fam);render();}
      catch(e){toast(e.message,true);}
    };
    document.querySelectorAll("[data-fam4]").forEach(b=>b.onclick=()=>setFamily(b.dataset.fam4,4));
    document.querySelectorAll("[data-fam6]").forEach(b=>b.onclick=()=>setFamily(b.dataset.fam6,6));
    document.querySelectorAll("[data-twin]").forEach(b=>b.onclick=async()=>{
      const fam=parseInt(b.dataset.fam,10)||0, want=fam===4?6:fam===6?4:4;
      if(fam===0&&!confirm("This target never stated an address family. Building the pair gives "+
        "the bare name to IPv6 and adds the IPv4 half beside it, as <name>-v4. "+
        "Refused if the name has no AAAA record. Continue?")) return;
      try{const r=await api("POST","/api/v1/admin/targets/"+b.dataset.twin+"/twin",{});
        toast(r.note||("Now measured over IPv"+want));render();}
      catch(e){toast(e.message,true);}
    });
    const setPin=async(id,ip)=>{
      try{await api("PATCH","/api/v1/admin/targets/"+id,{pin_ip:ip});
        toast(ip?("Target pinned to "+ip):"Address no longer pinned");render();}
      catch(e){toast(e.message,true);}
    };
    document.querySelectorAll("[data-alert]").forEach(b=>b.onclick=async()=>{
      try{await api("PATCH","/api/v1/admin/targets/"+b.dataset.alert,{alerts_off:b.dataset.off!=="1"});
        toast(b.dataset.off==="1"?"Alerting enabled for this target":"Alerting disabled for this target");render();}
      catch(e){toast(e.message,true);}
    });
    document.querySelectorAll("[data-pin]").forEach(b=>b.onclick=()=>setPin(b.dataset.pin,b.dataset.ip));
    document.querySelectorAll("[data-unpin]").forEach(b=>b.onclick=()=>setPin(b.dataset.unpin,""));
    document.querySelectorAll("[data-vis]").forEach(b=>b.onclick=async()=>{
      try{await api("PATCH","/api/v1/admin/targets/"+b.dataset.vis,{public:b.dataset.pub!=="1"});
        toast(b.dataset.pub==="1"?"Target is now private":"Target is now public");render();}
      catch(e){toast(e.message,true);}
    });
    document.querySelectorAll("[data-star]").forEach(b=>b.onclick=async()=>{
      const id=+b.dataset.star;F.has(id)?F.delete(id):F.add(id);
      try{await api("PUT","/api/v1/admin/featured",[...F]);toast("Critical targets updated");render();}
      catch(e){toast(e.message,true);}
    });
    document.querySelectorAll("[data-edit]").forEach(b=>b.onclick=()=>{
      EDIT=list.find(t=>t.id==b.dataset.edit); render(); window.scrollTo({top:0,behavior:"smooth"});
    });
    document.querySelectorAll("[data-share]").forEach(b=>b.onclick=async()=>{
      const days=prompt("Share this target with a read-only link — for a transit provider's ticket, "
        +"another AS's NOC, or a customer. They see this one target only: your percentiles, the loss "
        +"and the traceroutes taken when the path degraded.\n\nValid for how many days? (0 = no expiry)","30");
      if(days===null) return;
      const note=prompt("A note, so you remember who it is for (ticket number, AS, customer):","")||"";
      try{
        const l=await api("POST","/api/v1/admin/targets/"+b.dataset.share+"/share",
          {days:parseInt(days||"30",10),note});
        const box=document.createElement("tr");
        H.render(box,html`<td colspan="9"><div class="note" style="margin:0">
          <strong>Share link created</strong> — copy it now, it is not shown again:
          <div class="mono" style="margin:6px 0;word-break:break-all;font-size:12px">${l.url}</div>
          <button class="btn s" id="cpy">Copy</button>
          <span class="faint" style="font-size:11.5px;margin-left:8px">${l.expires_at?
            "expires "+new Date(l.expires_at*1000).toLocaleDateString():"no expiry"} ·
            revoke it in <a href="${pathOf("shares")}" data-goto="shares">Share links</a></span></div></td>`);
        b.closest("tr").parentNode.insertBefore(box,b.closest("tr").nextSibling);
        box.querySelector("#cpy").onclick=()=>{navigator.clipboard?.writeText(l.url);toast("Link copied");};
        box.querySelectorAll("[data-goto]").forEach(a=>a.onclick=e=>{e.preventDefault();go(a.dataset.goto);});
      }catch(e){toast(e.message,true);}
    });
    document.querySelectorAll("[data-advice]").forEach(b=>b.onclick=async()=>{
      const id=b.dataset.advice, host=b.closest("tr");
      b.disabled=true;
      try{
        const a=await api("GET","/api/v1/admin/targets/"+id+"/advice");
        const VERD={clean:"b-ok","rate limiting":"b-warn","real outages":"b-crit",
                    "scattered loss":"b-warn","not enough data":"b-n"};
        const box=document.createElement("tr");
        H.render(box,html`<td colspan="8"><div class="note" style="margin:0">
          <span class="badge ${VERD[a.verdict]||"b-n"}">${a.verdict}</span>
          ${a.loss_pct?html` <strong>${a.loss_pct.toFixed(2)} %</strong> loss over 24 h`:""}
          <div style="margin-top:6px">${a.detail}</div>
          ${(a.suggestions||[]).map((s,i)=>html`<div style="margin-top:8px">
            <strong>${s.label}</strong> — ${s.why}
            <button class="btn p s" data-apply="${id}" data-patch="${JSON.stringify(s.patch)}"
              style="margin-left:8px">Apply</button></div>`)}
          </div></td>`);
        host.parentNode.insertBefore(box,host.nextSibling);
        box.querySelectorAll("[data-apply]").forEach(ab=>ab.onclick=async()=>{
          try{await api("PATCH","/api/v1/admin/targets/"+ab.dataset.apply,JSON.parse(ab.dataset.patch));
            toast("Target settings updated");render();}catch(e){toast(e.message,true);}
        });
      }catch(e){toast(e.message,true);}
      b.disabled=false;
    });
    document.querySelectorAll("[data-now]").forEach(b=>b.onclick=async()=>{
      try{await api("POST","/api/v1/admin/targets/"+b.dataset.now+"/check");
        toast("Measurement requested: the result appears within a few seconds");}
      catch(e){toast(e.message,true);}
    });
    document.querySelectorAll("[data-trace]").forEach(b=>b.onclick=()=>{TR_TARGET=+b.dataset.trace;go("traces");});
    document.querySelectorAll("[data-del]").forEach(b=>b.onclick=async()=>{
      if(!confirm("Delete this target? It is archived: the measurements are kept, the name becomes free again.")) return;
      try{await api("DELETE","/api/v1/admin/targets/"+b.dataset.del);
        toast("Target deleted");render();}catch(e){toast(e.message,true);}
    });
  }catch(e){H.render($("#tlist"),html`<div class="note">${e.message}</div>`);}
}
