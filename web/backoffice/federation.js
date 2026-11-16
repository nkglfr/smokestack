/* smokestack back-office — federation.
 * Charge par admin.html, dans l'ordre : ces fichiers partagent une
 * portee globale, comme app.js et i18n.js du site public. */
/* -------------------------------------------------------- federation */
async function viewFed(m){
  H.render(m,html`<h2>Peers and pairing</h2>
    <p class="lead">Pairing requires a human decision on both sides. You send a justified request
      to the public URL of another instance; its administrator sees it here, compares your
      fingerprint with the one exchanged out of band, reads your justification, then accepts
      or declines. Showing a pairing publicly also requires both sides to agree.</p>
    <div class="note" style="margin:0 0 16px">
      <strong>Looking for networks to pair with?</strong> Operators running smokestack post the public
      URL of their instance in
      <a href="https://github.com/nkglfr/smokestack/discussions/categories/show-and-tell" target="_blank"
         rel="noreferrer">Show and tell</a> on the project's discussions. Add yours — your instance URL,
      your AS number and where you measure from — and pick the ones whose paths interest you.
      Pairing still needs a human decision on both sides, so publishing your URL commits you to nothing.
    </div>
    <div id="f">Loading…</div>`);
  try{
    const [id,pairings,peers,info,site]=await Promise.all([
      api("GET","/api/v1/admin/fed/identity"),
      api("GET","/api/v1/admin/fed/pairing"),
      api("GET","/api/v1/admin/fed/peers"),
      api("GET","/api/v1/fed/pairing/info").catch(()=>({min_justification:20})),
      api("GET","/api/v1/site").catch(()=>({}))
    ]);
    // A hidden pairing page answers 404 to a person, and a peer's back-office
    // takes the bare address just as well.
    const share=(site.hidden_pages||[]).includes("pairing")
      ?["Address to share",id.profile.url||""]:["Pairing page to share",(id.profile.url||"")+"/pairing"];
    const MINJ=info.min_justification||20;
    const inbound=(pairings||[]).filter(p=>p.direction==="in"&&p.state==="pending");
    const outbound=(pairings||[]).filter(p=>p.direction==="out");
    PENDING=inbound.length;

    const out=[html`<div class="card"><h3>Our identity</h3><div class="body"><table><tbody>
        <tr><td style="width:210px">AS number</td><td class="mono">${id.profile.asn||"— not set —"}</td></tr>
        <tr><td>Organisation</td><td>${id.profile.org||"—"}</td></tr>
        <tr><td>${share[0]}</td><td class="mono">${share[1]}</td></tr>
        <tr><td>Fingerprint</td><td class="mono fp">${id.fingerprint}</td></tr>
        <tr><td>Declared anchors</td><td class="mono">${(id.profile.anchors||[]).join(", ")||"—"}</td></tr>
      </tbody></table></div></div>`];

    out.push(html`<div class="card"><h3>Incoming requests${(inbound.length?html` <span class="badge b-crit">${inbound.length}</span>`:"")}</h3><div class="body">`);
    out.push(inbound.length? inbound.map(p=>html`<div class="pend">
      <div style="display:flex;align-items:baseline;gap:9px;flex-wrap:wrap">
        <strong>${p.org||"(no name)"}</strong><span class="mono">${p.asn}</span>
        ${p.contact_name?html`<span style="font-size:12px;color:var(--ink2)">· ${p.contact_name}</span>`:""}
        <span style="margin-left:auto;font-size:12px;color:var(--ink2)">${dt(p.created_at)}</span>
      </div>
      <div class="mono" style="font-size:12px;color:var(--ink2);margin-top:3px">${p.url}</div>
      <div style="margin-top:8px;font-size:12px">Announced fingerprint: <span class="mono fp">${p.fingerprint}</span></div>
      <div class="quote"><span class="lbl">Justification</span>${p.justification}</div>
      ${p.private_note?html`<div class="quote priv"><span class="lbl">Private note for you</span>${p.private_note}</div>`:""}
      <div style="font-size:12px;color:var(--ink2)">Anchors: <span class="mono">${(p.anchors||[]).join(", ")||"—"}</span>
        · Public listing: ${p.public_listing?html`<span class="badge b-ok">accepted by the requester</span>`:html`<span class="badge b-n">declined by the requester</span>`}</div>
      <div class="field" style="margin-top:11px"><label>Fingerprint as the operator gave it to you
        <span style="color:var(--crit)">*</span> — required to accept</label>
        <input class="mono" id="fpc${p.id}" placeholder="0000-0000-0000-0000" autocomplete="off">
        <small style="color:var(--ink2)">Type what you were told by phone, on a peering list or at an
          exchange. A signature only proves the sender holds that key, not who they are: this comparison
          is what ties the key to a real network.</small></div>
      <div class="field"><label>Private reply to the requester (optional)</label>
        <textarea rows="2" id="rep${p.id}" placeholder="Welcome! Our anchors answer over IPv4 and IPv6."></textarea></div>
      <label class="chk"><input type="checkbox" id="pub${p.id}" ${p.public_listing?"checked":"disabled"}>
        <span>Show this pairing on our public page<small>${p.public_listing
          ?"The requester agrees. It is shown only if you tick this too."
          :"Not possible: the requester did not agree."}</small></span></label>
      <div class="acts">
        <button class="btn p s" data-acc="${p.id}">Accept the pairing</button>
        <button class="btn d s" data-rej="${p.id}">Decline</button>
      </div></div>`) : html`<div class="empty">No pending request.</div>`);
    out.push(html`</div></div>`);

    out.push(html`<div class="card"><h3>Request a pairing</h3><div class="body">
      <div class="field"><label>Public URL of the other instance</label>
        <input id="purl" placeholder="https://smokestack.example.net/pairing"></div>
      <div class="field"><label>Justification <span style="color:var(--crit)">*</span>
        — shown to the remote administrator, ${MINJ} characters minimum</label>
        <textarea id="pjust" rows="3" placeholder="We both peer at France-IX Paris and LyonIX and share many business customers: cross-monitoring our anchors would help us qualify incidents faster."></textarea>
        <div class="counter" id="pjc">0 / ${MINJ}</div></div>
      <div class="field"><label>Private note for the remote administrator (optional)</label>
        <textarea id="pnote" rows="2" placeholder="Hello, following our chat at the last RIPE meeting…"></textarea></div>
      <div class="row2"><div class="field"><label>Signing contact</label><input id="pcontact" value="${ME.display_name}"></div></div>
      <label class="chk"><input type="checkbox" id="ppub" checked>
        <span>Agree to this pairing being listed publicly<small>On both sides, listing requires both parties to agree.
          You can hide it later.</small></span></label>
      <button class="btn p" id="pgo">Send the request</button>
      <div class="note" style="margin-top:12px">The justification and the private note are only sent to the
        remote administrator. They never appear on any public page.</div>
    </div></div>`);

    out.push(html`<div class="card"><h3>Sent requests</h3><div class="body">`);
    out.push(outbound.length? outbound.map(p=>html`<div style="padding:10px 0;border-top:1px solid var(--line2)">
      <div style="display:flex;gap:9px;align-items:baseline;flex-wrap:wrap">
        <span class="mono" style="font-weight:600">${p.asn}</span><span>${p.org}</span>
        <span class="badge ${p.state==="accepted"?"b-ok":p.state==="pending"?"b-warn":"b-crit"}">${p.state}</span>
        <span style="margin-left:auto;font-size:12px;color:var(--ink2)">${dt(p.created_at)}</span></div>
      <div class="mono" style="font-size:11.5px;color:var(--ink3);margin-top:2px">remote fingerprint ${p.fingerprint}</div>
      <div class="quote"><span class="lbl">Your justification</span>${p.justification}</div>
      ${p.reply_note?html`<div class="quote priv"><span class="lbl">Reply from ${p.org}</span>${p.reply_note}</div>`:""}
      ${p.error?html`<div style="font-size:12px;color:var(--crit)">${p.error}</div>`:""}
    </div>`) : html`<div class="empty">No request sent.</div>`);
    out.push(html`</div></div>`);

    out.push(html`<div class="card"><h3>Peers</h3><div class="body">`);
    out.push((peers&&peers.length)?html`<table><thead><tr><th>AS</th><th>Organisation</th>
      <th>Fingerprint</th><th>Public</th><th>Last seen</th><th></th></tr></thead><tbody>${peers.map(p=>html`<tr><td class="mono">${p.asn}</td><td>${p.org}
        <div style="font-size:11.5px;color:var(--ink3)">${p.noc_email||""}</div></td>
        <td class="mono" style="font-size:12px">${p.fingerprint}</td>
        <td><label class="chk" style="margin:0"><input type="checkbox" data-pubtog="${p.id}"
          ${p.public?"checked":""} ${p.peer_consent?"":"disabled"}>
          <span style="font-size:12px">${p.peer_consent?(p.public?"listed":"hidden"):"no consent"}</span></label></td>
        <td style="font-size:12px">${dt(p.last_seen_at)}</td>
        <td style="text-align:right"><button class="btn s" data-prot="${p.id}">Rotate key</button>
          <button class="btn d s" data-pdel="${p.id}">Remove</button></td></tr>${
        p.last_error?html`<tr><td colspan="6" style="font-size:11px;color:var(--crit);border-top:none;padding-top:0">${p.last_error}</td></tr>`:""}`
      )}</tbody></table>`:html`<div class="empty">No peer yet.</div>`);
    out.push(html`</div></div>`);
    H.render($("#f"),out);

    const jc=()=>{const n=[...$("#pjust").value.trim()].length;
      $("#pjc").textContent=n+" / "+MINJ;$("#pjc").className="counter"+(n<MINJ?" bad":"");};
    $("#pjust").oninput=jc;jc();
    $("#pgo").onclick=async()=>{
      try{
        const r=await api("POST","/api/v1/admin/fed/pairing",{
          url:$("#purl").value,justification:$("#pjust").value,
          private_note:$("#pnote").value,contact_name:$("#pcontact").value,
          public_listing:$("#ppub").checked});
        toast("Request sent, remote fingerprint "+r.pairing.fingerprint);render();
      }catch(e){toast(e.message,true);}
    };
    document.querySelectorAll("[data-acc]").forEach(b=>b.onclick=async()=>{
      const id=b.dataset.acc;
      const fp=($("#fpc"+id).value||"").trim();
      if(!fp){toast("Enter the fingerprint you received out of band",true);$("#fpc"+id).focus();return;}
      try{await api("POST","/api/v1/admin/fed/pairing/"+id+"/decide",
        {accept:true,fingerprint:fp,reply:$("#rep"+id).value,public_listing:$("#pub"+id).checked});
        toast("Pairing accepted");render();}catch(e){toast(e.message,true);}
    });
    document.querySelectorAll("[data-rej]").forEach(b=>b.onclick=async()=>{
      const id=b.dataset.rej;
      try{await api("POST","/api/v1/admin/fed/pairing/"+id+"/decide",
        {accept:false,reply:$("#rep"+id).value});toast("Request declined");render();}
      catch(e){toast(e.message,true);}
    });
    document.querySelectorAll("[data-pubtog]").forEach(c=>c.onchange=async()=>{
      try{await api("PATCH","/api/v1/admin/fed/peers/"+c.dataset.pubtog,{public:c.checked});
        toast(c.checked?"Peer listed publicly":"Peer hidden");render();}
      catch(e){toast(e.message,true);render();}
    });
    document.querySelectorAll("[data-prot]").forEach(b=>b.onclick=async()=>{
      const fp=prompt("This peer's new fingerprint, as its operator gave it to you out of band:");
      if(!fp) return;
      try{await api("POST","/api/v1/admin/fed/peers/"+b.dataset.prot+"/rotate",{fingerprint:fp});
        toast("Key rotated");render();}catch(e){toast(e.message,true);}
    });
    document.querySelectorAll("[data-pdel]").forEach(b=>b.onclick=async()=>{
      if(!confirm("Remove this peer? Measurements towards its anchors stay in the database.")) return;
      try{await api("DELETE","/api/v1/admin/fed/peers/"+b.dataset.pdel);
        toast("Peer removed");render();}catch(e){toast(e.message,true);}
    });
  }catch(e){H.render($("#f"),html`<div class="note">${e.message}</div>`);}
}
