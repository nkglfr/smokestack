/* smokestack back-office — navigation, layout and view dispatch.
 * Charge par admin.html, dans l'ordre : ces fichiers partagent une
 * portee globale, comme app.js et i18n.js du site public. */
/* ------------------------------------------------------------ layout */
const NAV=[
  {grp:"Monitoring"},
  {id:"dash",   label:"Dashboard", min:1},
  {id:"targets",label:"Targets and categories", min:2},
  {id:"traces", label:"Traceroutes", min:1},
  {id:"break",  label:"Response-time breakdown", min:3},
  {id:"maint",  label:"Maintenance calendar", min:2},
  {id:"messages",label:"Messages", min:3, mbadge:true},
  {grp:"Federation"},
  {id:"fed",    label:"Peers and pairing", min:3, badge:true},
  {id:"notify", label:"NOC alerting (federation)", min:3},
  {id:"dc",     label:"Double-check with peers", min:3},
  {id:"alerts", label:"My targets alerting", min:3},
  {id:"certs",  label:"TLS certificates", min:3},
  {id:"channels", label:"Notification channels", min:3},
  {grp:"Instance"},
  {id:"probes", label:"Probes", min:3},
  {id:"storage",label:"Storage", min:3},
  {id:"site",   label:"Publisher page", min:3},
  {id:"net",    label:"Host network", min:3},
  {id:"langs",  label:"Languages", min:3},
  {id:"update", label:"Updates", min:4},
  {grp:"Accounts"},
  {id:"users",  label:"Users", min:4},
  {id:"apikeys",label:"API keys", min:1},
  {id:"shares", label:"Share links", min:2},
  {id:"logs",   label:"Service log", min:3},
  {id:"audit",  label:"Audit log", min:3}
];
const LVL={viewer:1,editor:2,admin:3,master:4};

function shell(){
  const nav=[];
  for(const n of NAV){
    if(n.grp){nav.push(html`<div class="grp">${n.grp}</div>`);continue;}
    if(LVL[ME.role]<n.min) continue;
    nav.push(html`<button data-v="${n.id}" aria-current="${VIEW===n.id?"true":"false"}">${n.label}${
      n.badge&&PENDING?html`<span class="n">${PENDING}</span>`:""}${
      n.mbadge&&UNREAD?html`<span class="n">${UNREAD}</span>`:""}</button>`);
  }
  H.render($("#app"),html`
    <div class="top"><div class="topin">
      <button class="btn s burger" id="nav-burger" aria-label="Menu" aria-expanded="false">☰</button>
      <div class="brand" id="home" style="cursor:pointer" title="Dashboard"><span class="logo">S</span><span>smokestack</span>
        <span class="badge b-n" style="margin-left:6px">admin</span></div>
      <span class="spacer"></span>
      <div class="who"><div>${ME.display_name}</div>
        <div><span class="badge b-n">${ME.role}</span></div></div>
      <button class="btn s" id="pw">Password</button>
      <button class="btn s" id="out">Sign out</button>
    </div></div>
    <div class="shell"><nav class="side" id="side">${nav}</nav><main id="main"></main></div>
    <div class="scrim" id="scrim" hidden></div>`);
  $("#home").onclick=()=>{EDIT=null;go("dash");};
  $("#out").onclick=async()=>{await api("POST","/api/v1/auth/logout");location.reload();};
  $("#pw").onclick=changePassword;
  // The sidebar slides over the page on small screens and closes as soon
  // as a screen is chosen, so the content is never hidden behind it.
  const side=$("#side"), scrim=$("#scrim"), nb=$("#nav-burger");
  const closeNav=()=>{side.classList.remove("open");scrim.hidden=true;nb.setAttribute("aria-expanded","false");};
  document.querySelectorAll("[data-v]").forEach(b=>b.onclick=()=>{closeNav();go(b.dataset.v);});
  nb.onclick=e=>{e.stopPropagation();const open=!side.classList.contains("open");
    side.classList.toggle("open",open);scrim.hidden=!open;nb.setAttribute("aria-expanded",String(open));};
  scrim.onclick=closeNav;
  document.addEventListener("keydown",e=>{if(e.key==="Escape")closeNav();});
  addEventListener("resize",()=>{if(innerWidth>=900)closeNav();});
  render();
}
/* ------------------------------------------------------------ routes */
// Le serveur sert cette page pour tout /admin/… : une vue peut donc
// avoir sa propre adresse sans rien changer cote serveur. Rafraichir
// reste sur place, le bouton precedent fonctionne, et une adresse se
// colle dans un message a un collegue.
const VIEWS = NAV.filter(n => n.id);
const pathOf = view => view === "dash" ? "/admin" : "/admin/" + view;

// La vue que l'adresse demande, si elle existe et si le role la permet.
// Une adresse perimee ou interdite ne doit pas donner une coquille vide :
// on retombe sur le tableau de bord, et l'adresse suit.
function viewFromPath() {
  const seg = (location.pathname.match(/^\/admin\/([a-z]+)\/?$/) || [])[1] || "dash";
  const n = VIEWS.find(v => v.id === seg);
  if (!n) return "dash";
  if (ME && LVL[ME.role] < n.min) return "dash";
  return seg;
}

function go(view, push) {
  VIEW = view;
  if (view !== "targets") EDIT = null;
  const url = pathOf(view) + location.search;
  if (push === false || location.pathname === pathOf(view)) history.replaceState(null, "", url);
  else history.pushState(null, "", url);
  shell();
}

// Le navigateur a change d'adresse sans recharger : on suit, sans
// re-empiler l'historique.
addEventListener("popstate", () => {
  if (!ME) return;
  VIEW = viewFromPath();
  shell();
});

async function changePassword(){
  const cur=prompt("Current password");if(!cur) return;
  const nw=prompt("New password (12 characters minimum)");if(!nw) return;
  try{
    await api("POST","/api/v1/auth/password",{current:cur,new:nw});
    toast("Password changed, please sign in again");
    setTimeout(()=>location.reload(),1200);
  }catch(e){toast(e.message,true);}
}

function render(){
  const m=$("#main");
  const views={dash:viewDash,dc:viewDC,targets:viewTargets,traces:viewTraces,maint:viewMaint,messages:viewMessages,alerts:viewAlerts,certs:viewCerts,channels:viewChannels,fed:viewFed,notify:viewNotify,
    break:viewBreak,probes:viewProbes,storage:viewStorage,site:viewSite,net:viewNet,langs:viewLangs,update:viewUpdate,users:viewUsers,apikeys:viewAPIKeys,shares:viewShares,logs:viewLogs,audit:viewAudit};
  return (views[VIEW]||viewDash)(m);
}
