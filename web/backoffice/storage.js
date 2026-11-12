/* smokestack back-office — storage and retention.
 * Charge par admin.html, dans l'ordre : ces fichiers partagent une
 * portee globale, comme app.js et i18n.js du site public. */
/* ----------------------------------------------------------- storage */
async function viewStorage(m){
  H.render(m,html`<h2>Storage</h2>
    <p class="lead">Two different things rotate here. The raw <em>archive</em> is rotated by the
      policy below, oldest first, and chunks already on S3 go first since they remain retrievable.
      How long each <em>resolution</em> is kept in the database is the second card, and that one
      deletes measurements outright.</p>
    <div id="s">Loading…</div>
    <div id="ret"></div>`);
  retentionCard();
  try{
    const d=await api("GET","/api/v1/admin/storage");
    const c=d.config,r=d.report||{},s3=c.s3||{},lo=c.local||{};
    H.render($("#s"),html`<div class="card"><h3>Mode</h3><div class="body">
      <div class="field"><label>Policy</label><select id="mode">
        <option value="local" ${c.mode==="local"?"selected":""}>Local only (capacity-based rotation)</option>
        <option value="s3" ${c.mode==="s3"?"selected":""}>S3 first</option>
        <option value="hybrid" ${c.mode==="hybrid"?"selected":""}>Hybrid (local, then S3)</option>
      </select></div></div></div>
      <div class="card"><h3>S3 target</h3><div class="body"><div class="row2">
        <div class="field"><label>Endpoint</label><input id="ep" value="${s3.endpoint||""}" placeholder="s3.fr-par.scw.cloud"></div>
        <div class="field"><label>Bucket</label><input id="bk" value="${s3.bucket||""}"></div>
        <div class="field"><label>Prefix</label><input id="px" value="${s3.prefix||""}"></div>
        <div class="field"><label>Region</label><input id="rg" value="${s3.region||""}"></div>
        <div class="field"><label>Access key</label><input id="ak" value="${s3.access_key||""}"></div>
        <div class="field"><label>Secret key</label><input id="sk" type="password" placeholder="unchanged"></div>
        <div class="field"><label>Storage class</label><input id="cl" value="${s3.storage_class||""}"></div>
        <label class="chk"><input type="checkbox" id="ps" ${s3.path_style?"checked":""}><span>Path-style URLs (MinIO, Ceph)</span></label>
      </div></div></div>
      <div class="card"><h3>Local rotation</h3><div class="body"><div class="row2">
        <div class="field"><label>Quota (GB)</label><input id="q" type="number" step="0.5"
          value="${((lo.quota_bytes||0)/1073741824).toFixed(1)}"></div>
        <div class="field"><label>High watermark</label><input id="hw" type="number" step="0.01" value="${lo.high_watermark||0.85}"></div>
        <div class="field"><label>Low watermark</label><input id="lw" type="number" step="0.01" value="${lo.low_watermark||0.7}"></div>
        <div class="field"><label>Keep locally (hours)</label><input id="kl" type="number" value="${lo.keep_local_hours||48}"></div>
      </div>
      <button class="btn p" id="save">Save</button></div></div>
      <div class="card"><h3>Status</h3><div class="body"><table><tbody>
        <tr><td style="width:210px">Local archive</td><td>${gb(r.local_bytes||0)} / ${gb(r.quota_bytes||0)}</td></tr>
        <tr><td>Disk usage</td><td>${((r.disk_used_pct||0)*100).toFixed(1)} %
          ${r.degraded?html`<span class="badge b-crit">degraded mode</span>`:""}</td></tr>
        <tr><td>Chunks</td><td>${r.chunks||0}</td></tr>
        <tr><td>Pending uploads</td><td>${r.pending_uploads||0}</td></tr>
        <tr><td>Projected retention</td><td>${r.projected_days?r.projected_days.toFixed(0)+" days":"—"}</td></tr>
      </tbody></table></div></div>`);
    document.querySelectorAll("[data-goto]").forEach(a=>a.onclick=e=>{e.preventDefault();go(a.dataset.goto);});
    $("#save").onclick=async()=>{
      try{
        await api("PUT","/api/v1/admin/storage",{
          mode:$("#mode").value,
          s3:{endpoint:$("#ep").value,bucket:$("#bk").value,prefix:$("#px").value,
              region:$("#rg").value,access_key:$("#ak").value,secret_key:$("#sk").value,
              path_style:$("#ps").checked,storage_class:$("#cl").value},
          local:{quota_bytes:Math.round(parseFloat($("#q").value||0)*1073741824),
                 high_watermark:parseFloat($("#hw").value),
                 low_watermark:parseFloat($("#lw").value),
                 keep_local_hours:parseInt($("#kl").value,10)}});
        toast("Saved");render();
      }catch(e){toast(e.message,true);}
    };
  }catch(e){H.render($("#s"),html`<div class="note">${e.message}</div>`);}
}

/* ------------------------------------------------------- retention (#115)
 * Separate from the rotation policy above on purpose: that one moves raw
 * chunks between disk and S3 and nothing is lost, this one deletes rows.
 *
 * Saving takes two clicks when something would be deleted, and one when
 * nothing would. The confirmation holds the configuration that was previewed
 * rather than re-reading the inputs, so what is saved is what was shown — an
 * operator who edits a field after seeing the cost has to ask again. */
let retPending=null;

async function retentionCard(){
  try{
    const d=await api("GET","/api/v1/admin/retention");
    const c=d.config||{},fl=d.floors||{};
    const row=(id,label,key,note)=>html`<div class="field">
      <label for="${id}">${label}</label>
      <input id="${id}" type="number" min="0" step="1" value="${c[key]==null?0:c[key]}">
      <small>${note}</small></div>`;
    H.render($("#ret"),html`<div class="card"><h3>Retention per resolution</h3><div class="body">
      <p>Days of history kept at each resolution. <strong>0 means unlimited.</strong>
        A coarser resolution cannot be kept for less time than a finer one, and none can be
        kept for less than the window that rolls it up.</p>
      <div class="row2">
        ${row("rsamples","Raw samples","samples",`at least ${fl.samples||1} day`)}
        ${row("r1m","1-minute buckets","roll_1m",`at least ${fl.roll_1m||1} day`)}
        ${row("r5m","5-minute buckets","roll_5m",`at least ${fl.roll_5m||3} days`)}
        ${row("r1h","Hourly buckets","roll_1h",`at least ${fl.roll_1h||3} days`)}
        ${row("r1d","Daily buckets","roll_1d",`at least ${fl.roll_1d||1} day`)}
      </div>
      <div id="rprev"></div>
      <button class="btn p" id="rsave">Check what this changes</button>
      <button class="btn" id="rreset" style="margin-left:8px">Back to the defaults</button>
    </div></div>`);

    const n=id=>{const v=parseInt($(id).value,10);return isNaN(v)||v<0?0:v;};
    const read=()=>({samples:n("#rsamples"),roll_1m:n("#r1m"),roll_5m:n("#r5m"),
                     roll_1h:n("#r1h"),roll_1d:n("#r1d")});

    const clear=()=>{retPending=null;H.render($("#rprev"),html``);
                     $("#rsave").textContent="Check what this changes";};
    ["#rsamples","#r1m","#r5m","#r1h","#r1d"].forEach(id=>$(id).oninput=clear);

    $("#rreset").onclick=()=>{
      const dd=d.defaults||{};
      $("#rsamples").value=dd.samples||0;$("#r1m").value=dd.roll_1m||0;
      $("#r5m").value=dd.roll_5m||0;$("#r1h").value=dd.roll_1h||0;
      $("#r1d").value=dd.roll_1d||0;clear();
    };

    $("#rsave").onclick=async()=>{
      // Second click: save exactly what the first one described.
      if(retPending){
        try{await api("PUT","/api/v1/admin/retention",retPending);
            toast("Saved");retPending=null;retentionCard();}
        catch(e){toast(e.message,true);}
        return;
      }
      const want=read();
      try{
        const p=await api("POST","/api/v1/admin/retention/preview",want);
        const ch=p.changes||[];
        if(!ch.length){
          await api("PUT","/api/v1/admin/retention",want);
          toast("Saved");retentionCard();return;
        }
        retPending=want;
        H.render($("#rprev"),html`<div class="note" style="border-color:var(--crit)">
          <strong>The next purge would delete this, and it cannot be undone.</strong>
          <table><thead><tr><th>Resolution</th><th>From</th><th>To</th><th>Rows deleted</th></tr></thead>
          <tbody>${ch.map(x=>html`<tr>
            <td>${x.table}</td><td>${x.from}</td><td>${x.to}</td><td>${x.rows}</td></tr>`)}</tbody></table>
          ${ch.filter(x=>x.breaks_backfill_for).map(x=>html`<p>Shortening <strong>${x.table}</strong>
            also removes the only source for rebuilding <strong>${x.breaks_backfill_for}</strong>
            over that period. After this, the longer views cannot be recovered for it, ever.</p>`)}
        </div>`);
        $("#rsave").textContent="Delete it and save";
      }catch(e){toast(e.message,true);}
    };
  }catch(e){H.render($("#ret"),html`<div class="note">${e.message}</div>`);}
}
