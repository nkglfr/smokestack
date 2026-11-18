/* smokestack back-office — API keys.
 * Loaded by admin.html in order: these files share a global scope, like app.js and i18n.js on the public site. */
/* ----------------------------------------------------------- API keys */
async function viewAPIKeys(m){
  const all=LVL[ME.role]>=LVL.admin;
  // A key is capped at admin whatever the account, and at the account's own role below that.
  const roles=["viewer","editor","admin"].filter(r=>LVL[r]<=LVL[ME.role]);
  H.render(m,html`<h2>API keys</h2>
    <p class="lead">A key lets a script call the API as you: Ansible, Terraform, a CI job, a Prometheus scraper.
      It is sent as <span class="mono">Authorization: Bearer ssk_…</span> and needs no CSRF token.
      It acts with its own role or your account's, whichever is lower: demoting your account demotes its keys,
      and disabling or deleting it deletes them. A key never acts as master, cannot create accounts or keys,
      and cannot change your password.</p>
    <p class="lead">A key is shown once, when it is created; only its hash is stored. Each audit log entry written for a
      request made with it names your account and the key. Revoking it does not withdraw what was made with it,
      such as share links or probe tokens.${all?" As an admin, you see and can revoke the keys of every account.":""}</p>
    <div id="kl">Loading…</div>
    <div class="card"><h3>New key</h3><div class="body"><div class="row2">
      <div class="field"><label>Name</label><input id="kname" placeholder="ansible" autocomplete="off"></div>
      <div class="field"><label>Role</label><select id="krole">${roles.map(r=>html`<option>${r}</option>`)}</select></div>
      <div class="field"><label>Expires after (days)</label><input id="kdays" type="number" min="1" max="3650" placeholder="never"></div>
    </div><button class="btn p" id="kadd">Create the key</button></div></div>`);

  // The key lives only in the argument of load() and in the DOM it renders, and the next render drops it. It is drawn
  // whether the list reload succeeds or not: the hash is already stored, so a key not shown is a key nobody will ever have.
  const keyCard=k=>k?html`<div class="card" style="border-color:var(--crit)"><h3>Key ${k.name}</h3><div class="body">
      <p style="margin:0 0 10px"><strong>Copy this key now.</strong> It is shown once and will not be shown again,
        because only its hash is stored. If you lose it, revoke it and create another.</p>
      <div class="mono" style="padding:10px 12px;background:var(--line2);border-radius:var(--radius-s);word-break:break-all;user-select:all">${k.token}</div>
    </div></div>`:"";
  const state=k=>!k.effective_role?html`<span class="badge b-crit">expired</span>`
    :k.effective_role!==k.role?html`<span class="badge b-warn" title="The account's role is lower than the key's">acts as ${k.effective_role}</span>`
    :html`<span class="badge b-ok">active</span>`;

  const load=async created=>{
    let list;
    try{list=(await api("GET","/api/v1/admin/apikeys"))||[];}
    catch(e){H.render($("#kl"),html`${keyCard(created)}<div class="note">${e.message}</div>`);return;}
    H.render($("#kl"),html`${keyCard(created)}
      <div class="card"><h3>${all?"Keys of every account":"Your keys"}</h3><div class="body">
        ${list.length?html`<table class="resp"><thead><tr><th>Name</th>${all?html`<th>Account</th>`:""}<th>Role</th>
          <th>Created</th><th>Expires</th><th>Last used</th><th></th></tr></thead><tbody>${list.map(k=>html`<tr>
          <td class="ttl" data-l=""><div>${k.name}</div><div class="mono" style="font-size:11.5px;color:var(--ink3)">${k.hint}…</div></td>
          ${all?html`<td data-l="Account">${k.owner}</td>`:""}
          <td data-l="Role">${k.role} ${state(k)}</td>
          <td data-l="Created">${dt(k.created_at)}</td>
          <td data-l="Expires">${k.expires_at?dt(k.expires_at):"never"}</td>
          <td data-l="Last used">${dt(k.last_used)}</td>
          <td class="acts" data-l=""><button class="btn d s" data-krev="${k.id}" data-name="${k.name}">Revoke</button></td>
          </tr>`)}</tbody></table>`:html`<div class="empty">No API key.</div>`}
      </div></div>`);
    document.querySelectorAll("[data-krev]").forEach(b=>b.onclick=async()=>{
      if(!confirm("Revoke the key "+b.dataset.name+"? Whatever uses it is refused from now on.")) return;
      try{await api("DELETE","/api/v1/admin/apikeys/"+b.dataset.krev);toast("Key revoked");await load();}
      catch(e){toast(e.message,true);}
    });
  };

  $("#kadd").onclick=async()=>{
    const days=$("#kdays").value.trim();
    try{
      const k=await api("POST","/api/v1/admin/apikeys",{name:$("#kname").value,role:$("#krole").value,
        days:days?parseInt(days,10)||0:0});
      $("#kname").value="";$("#kdays").value="";
      await load(k);
    }catch(e){toast(e.message,true);}
  };
  await load();
}
