/* smokestack back-office — remote probe enrolment.
 * Loaded by admin.html in order: these files share a global scope, like app.js and i18n.js on the public site. */
/* ------------------------------------------------------------- probes */
async function viewProbes(m){
  H.render(m,html`<h2>Probes</h2>
    <p class="lead">A remote probe measures from another location and authenticates to this instance with a token.
      The local probe writes through the Unix socket and takes no token. A token is shown once, when it is issued;
      only its hash is stored, so it cannot be shown again. Rotating a token issues a new one and the old one stops working.</p>
    <div id="pl">Loading…</div>
    <div class="card"><h3>Add a probe</h3><div class="body">
      <div class="field"><label>Slug</label><input id="pslug" class="mono" placeholder="paris-1" autocomplete="off"></div>
      <div class="field"><label>Name</label><input id="pname" placeholder="Paris 1" autocomplete="off"></div>
      <div class="field"><label>Location</label><input id="ploc" placeholder="Paris, FR" autocomplete="off"></div>
      <div class="field"><label>Address (optional)</label><input id="paddr" class="mono" placeholder="203.0.113.7" autocomplete="off">
        <div class="note" style="margin-top:6px">Only needed when the observed address would be wrong: a probe behind NAT, or an instance behind a reverse proxy, where what arrives is the proxy.
          Leave empty to use the address seen when the probe connects.</div></div>
      <button class="btn p" id="padd">Add probe</button>
      <div class="note" style="margin-top:12px">The slug takes lowercase letters, digits and inner hyphens, 2 to 39 characters.
        A new probe has no token and cannot send anything until you issue one.</div>
    </div></div>`);

  // The token lives only in the argument of this call and in the DOM it renders. It is never stored anywhere else, and the next render drops it.
  //
  // It is built separately from the list on purpose. The server has already stored the hash by the time we get here, so a token that is not shown
  // is a token nobody will ever know, and the probe would have to be rotated again to recover. Reloading the list must therefore never be able to
  // swallow it: the block is rendered whether that reload succeeds or fails.
  const tokenCard=issued=>issued?html`<div class="card" style="border-color:var(--crit)"><h3>Token for ${issued.slug}</h3><div class="body">
        <p style="margin:0 0 10px"><strong>Copy this token now.</strong> It is shown once and will not be shown again,
          because only its hash is stored. If you lose it, rotate the token to issue a new one.</p>
        <div class="mono" style="padding:10px 12px;background:var(--line2);border-radius:var(--radius-s);word-break:break-all;user-select:all">${issued.token}</div>
      </div></div>`:"";

  const load=async(issued)=>{
    let list;
    try{list=(await api("GET","/api/v1/admin/probes")).probes||[];}
    catch(e){H.render($("#pl"),html`${tokenCard(issued)}<div class="note">${e.message}</div>`);return;}
    // Assigned-target count per non-local probe. The list endpoint does not return it, so it is one GET per probe (a handful at most).
    // A failed fetch leaves null: the button then shows no count and the "no targets" badge is not claimed, since we do not know.
    const counts={};
    await Promise.all(list.filter(p=>!p.local).map(async p=>{
      try{counts[p.id]=((await api("GET","/api/v1/admin/probes/"+p.id+"/targets")).targets||[]).length;}
      catch(e){counts[p.id]=null;}
    }));
    // Where the probe measures from. A non-empty `address` is the operator's statement and overrides what was observed; `from` is already resolved server-side.
    const fromCell=p=>{
      const shown=p.from||p.address||p.seen_address;
      if(!shown) return html`—`;
      return html`<span class="mono">${shown}</span>${p.asn?html` <span class="badge b-n">${p.asn}</span>`:""}
        ${p.address?html` <span class="badge b-n">stated</span>`:""}
        ${p.address&&p.seen_address&&p.seen_address!==p.address?html`<div style="font-size:11.5px;color:var(--ink3)">observed: <span class="mono">${p.seen_address}</span></div>`:""}`;
    };
    const row=p=>html`<tr>
      <td class="mono">${p.slug}</td><td>${p.name}</td><td>${p.location||"—"}</td>
      <td>${dt(p.last_seen_at)}</td>
      <td>${fromCell(p)}</td>
      <td>${p.local?html`<span class="badge b-n">local</span> `:""}
        ${p.enabled?html`<span class="badge b-ok">enabled</span>`:html`<span class="badge b-warn">disabled</span>`}
        ${p.local?"":p.has_token?html` <span class="badge b-ok">token set</span>`:html` <span class="badge b-crit">no token</span>`}
        ${p.local||p.all_targets?"":counts[p.id]!==0?"":html` <span class="badge b-warn" title="This probe has no assigned target, so it measures nothing">no targets</span>`}
        ${p.local||!p.all_targets?"":html` <span class="badge b-n" title="This probe measures every target, including ones added later">all targets</span>`}</td>
      <td class="acts">${p.local?"":html`
        <button class="btn s" data-tok="${p.id}" data-slug="${p.slug}" data-had="${p.has_token?"1":"0"}">${p.has_token?"Rotate token":"Issue token"}</button>
        ${p.has_token?html`<button class="btn d s" data-rev="${p.id}" data-slug="${p.slug}">Revoke token</button>`:""}
        <button class="btn s" data-tg="${p.id}" data-slug="${p.slug}">${p.all_targets?"Targets (all)":counts[p.id]==null?"Targets":"Targets ("+counts[p.id]+")"}</button>
        <button class="btn s" data-addr="${p.id}" data-slug="${p.slug}" data-cur="${p.address||""}">Set address</button>
        ${p.address?html`<button class="btn s" data-addr-clear="${p.id}" data-slug="${p.slug}">Use observed</button>`:""}
        <button class="btn s" data-en="${p.id}" data-to="${p.enabled?"0":"1"}">${p.enabled?"Disable":"Enable"}</button>`}</td>
      </tr>`;
    H.render($("#pl"),html`
      ${tokenCard(issued)}
      <div class="card"><h3>Registered probes</h3><div class="body">
        ${list.length?html`<table><thead><tr><th>Slug</th><th>Name</th><th>Location</th><th>Last seen</th><th>Measures from</th><th>State</th><th></th></tr></thead>
          <tbody>${list.map(row)}</tbody></table>`:html`<div class="empty">No probe.</div>`}
      </div></div>
      <div id="pe"></div>`);
    // The assignment editor renders into its own block under the table, so opening or cancelling it never re-renders the list or drops a token shown above.
    document.querySelectorAll("[data-tg]").forEach(b=>b.onclick=async()=>{
      try{
        const [all,cur]=await Promise.all([api("GET","/api/v1/admin/targets"),api("GET","/api/v1/admin/probes/"+b.dataset.tg+"/targets")]);
        const targets=Array.isArray(all)?all:[]; // /admin/targets returns a bare array of targets, as targets.js reads it
        const on=new Set(cur.targets||[]);
        // A boolean prints nothing in the template engine, so the attribute is spelled out as a string.
        H.render($("#pe"),html`<div class="card"><h3>Targets measured by ${b.dataset.slug}</h3><div class="body">
          <label class="chk"><input type="checkbox" id="tgall" ${cur.all_targets?"checked":""}>
            <span>Measure every target<small>Including targets added later. The list below is kept and comes back if you turn this off.</small></span></label>
          <p style="margin:10px 0">A probe measures only the targets ticked here. With none ticked it measures nothing.</p>
          ${targets.length?targets.map(t=>html`<label class="chk"><input type="checkbox" data-tid="${t.id}" ${on.has(t.id)?"checked":""}>
            <span>${t.title}<small class="mono">${t.host}${t.port?":"+t.port:""}</small></span></label>`):html`<div class="empty">No target.</div>`}
          <div class="acts"><button class="btn p" id="tgsave">Save</button><button class="btn" id="tgcancel">Cancel</button></div>
        </div></div>`);
        // The list stays visible but inert while "every target" is on, so that what comes back when it is
        // turned off is on screen rather than a promise.
        const syncAll=()=>{const off=$("#tgall").checked;
          document.querySelectorAll("#pe [data-tid]").forEach(c=>c.disabled=off);};
        $("#tgall").onchange=syncAll; syncAll();
        $("#tgcancel").onclick=()=>H.render($("#pe"),""); // nothing sent
        $("#tgsave").onclick=async()=>{
          const ids=[...document.querySelectorAll("#pe [data-tid]")].filter(c=>c.checked).map(c=>Number(c.dataset.tid));
          try{
            // The list is written first and the flag second, so the list is preserved either way and the
            // probe is never briefly measuring nothing between the two requests.
            await api("PUT","/api/v1/admin/probes/"+b.dataset.tg+"/targets",{targets:ids});
            await api("PATCH","/api/v1/admin/probes/"+b.dataset.tg,{all_targets:$("#tgall").checked});
            toast("Targets saved");await load();
          }catch(e){toast(e.message,true);}
        };
      }catch(e){toast(e.message,true);}
    });
    document.querySelectorAll("[data-tok]").forEach(b=>b.onclick=async()=>{
      if(b.dataset.had==="1"&&!confirm("Rotate the token of "+b.dataset.slug+"? The current token stops working immediately.")) return;
      try{
        const r=await api("POST","/api/v1/admin/probes/"+b.dataset.tok+"/token");
        await load({slug:b.dataset.slug,token:r.token});
      }catch(e){toast(e.message,true);}
    });
    document.querySelectorAll("[data-rev]").forEach(b=>b.onclick=async()=>{
      if(!confirm("Revoke the token of "+b.dataset.slug+"? The probe can no longer send measurements until a new token is issued.")) return;
      try{await api("DELETE","/api/v1/admin/probes/"+b.dataset.rev+"/token");toast("Token revoked");await load();}
      catch(e){toast(e.message,true);}
    });
    // `address` is sent only when the operator actually edited it: an absent field changes nothing, an empty string clears the override.
    const patchAddr=async(id,address)=>{
      try{await api("PATCH","/api/v1/admin/probes/"+id,{address});toast(address===""?"Stated address cleared":"Address saved");await load();}
      catch(e){toast(e.message,true);}
    };
    document.querySelectorAll("[data-addr]").forEach(b=>b.onclick=()=>{
      const v=prompt("Address of "+b.dataset.slug+" (an IP address). Leave empty to use the observed address.",b.dataset.cur);
      if(v===null) return; // cancelled: nothing edited, nothing sent
      const next=v.trim();
      if(next===b.dataset.cur) return; // unchanged: do not send the field
      patchAddr(b.dataset.addr,next);
    });
    document.querySelectorAll("[data-addr-clear]").forEach(b=>b.onclick=()=>{
      if(!confirm("Clear the stated address of "+b.dataset.slug+"? The observed address will be used.")) return;
      patchAddr(b.dataset.addrClear,"");
    });
    document.querySelectorAll("[data-en]").forEach(b=>b.onclick=async()=>{
      const on=b.dataset.to==="1";
      try{await api("PATCH","/api/v1/admin/probes/"+b.dataset.en,{enabled:on});toast(on?"Probe enabled":"Probe disabled");await load();}
      catch(e){toast(e.message,true);}
    });
  };

  $("#padd").onclick=async()=>{
    try{
      const body={slug:$("#pslug").value,name:$("#pname").value,location:$("#ploc").value};
      const addr=$("#paddr").value.trim();
      if(addr) body.address=addr; // optional: omitted when empty
      await api("POST","/api/v1/admin/probes",body);
      ["#pslug","#pname","#ploc","#paddr"].forEach(s=>$(s).value="");
      toast("Probe added");await load();
    }catch(e){toast(e.message,true);}
  };
  await load();
}
