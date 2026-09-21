# smokestack

Supervision de latence type SmokePing : sonde ICMP/TCP, agrégats à
percentiles fusionnables, archive brute sur S3 ou en local avec rotation
par capacité, interface publique avec zoom.

Un seul binaire, une seule dépendance (`modernc.org/sqlite`, pure Go, sans
cgo). Le front est compilé dans l'exécutable : le processus ne sert aucun
fichier depuis le disque, donc la base SQLite n'est atteignable par aucune
URL, quelle que soit la configuration du reverse proxy.

## Installation

Guide complet en anglais : **[DEPLOY.md](../DEPLOY.md)** (installation, HTTPS,
mises à jour, publication des versions, sauvegardes, dépannage).

En une commande, depuis un paquet publié :

```bash
sudo sh install.sh --package smokestack-1.2.0-linux-amd64.zip --admin-email noc@exemple.fr
```

Le script vérifie le paquet, crée l'utilisateur système, installe le service
systemd durci, crée le compte master et affiche son mot de passe. Relancé sur
un serveur déjà installé, il met à jour en conservant configuration et données.

Depuis les sources (Go 1.22) :

```bash
go mod tidy                 # une fois, puis commiter go.sum
make build                  # binaire statique dans dist/
sudo ./install.sh --package dist/smokestack
```

Essai en premier plan, sans installation :

```bash
make build
./dist/smokestack -config ./config.json -listen 127.0.0.1:8080
# le code d'installation du compte master s'affiche dans le journal
```

## Ligne de commande

```
smokestack [-config FICHIER]                 lance le service web
smokestack probe [-config FICHIER]           lance la sonde isolée
smokestack version | selftest
smokestack user add -email E [-role master]  crée un compte (mot de passe généré)
smokestack update [-force] PAQUET.zip        installe une version
smokestack rollback                          revient à la version précédente
smokestack release keygen | pack | latest    outils de publication
```

Après `install.sh`, la commande `smokestack` s'exécute toujours sous
l'utilisateur du service, même lancée avec `sudo`.

## Mises à jour

Une version est un ZIP contenant le binaire, un `manifest.json` (version,
plateforme, SHA-256) et la signature Ed25519 du manifeste. Qu'elle soit
installée depuis le back-office (*Instance → Mise à jour*), en ligne de
commande ou automatiquement :

1. signature, plateforme et empreinte sont vérifiées ; un paquet non signé
   ou signé par une clé inconnue est refusé ;
2. le nouveau binaire passe son autotest ;
3. `config.db` est sauvegardée ;
4. le lien `current` bascule de façon atomique et le processus se relance en
   place (même PID) ;
5. si la nouvelle version échoue trois fois à démarrer, la précédente est
   restaurée automatiquement, et l'incident est inscrit dans l'historique.

Les mises à jour automatiques exigent toujours une signature de confiance.

## Dépôt et intégration continue

| Fichier | Rôle |
|---|---|
| `Makefile` | `build`, `test`, `dist` (paquets signés amd64/arm64), `release-key` |
| `.github/workflows/ci.yml` | vet, tests, autotest du binaire, syntaxe JS, installeur |
| `.github/workflows/release.yml` | sur tag `v*` : construit, signe, publie la release GitHub |
| `.github/dependabot.yml` | mises à jour hebdomadaires des dépendances et des actions |
| `release.pub` | clés publiques de confiance, compilées dans le binaire |

Publier une version : `git tag v1.3.0 && git push origin v1.3.0`. Les
instances en mise à jour automatique l'installent dans les heures qui suivent.

Les adresses du site officiel et du dépôt sont compilées dans le binaire
(`make dist OFFICIAL_URL=... REPO_URL=...`) ; la publication refuse de se
faire tant qu'elles contiennent `CHANGE-ME` (aujourd'hui : dépôt `github.com/nkglfr/smokestack`, qui sert aussi de site officiel).

## Isolation de la sonde

Par défaut, la sonde tourne dans **son propre processus** (`smokestack
probe`, service `smokestack-probe`), seul autorisé à ouvrir un socket brut
et prioritaire sur le CPU. Le service web, exposé à Internet, n'a plus ce
droit. Les deux dialoguent par un socket Unix.

- Horodatage des réponses par le noyau (`SO_TIMESTAMPNS`, `TCP_INFO`) : la
  charge du processus ne s'ajoute jamais au temps de réponse mesuré.
- La sonde ne touche pas la base : cibles en mémoire, mesures dans une file
  non bloquante, un écrivain unique par lots sur une connexion dédiée.
- Accueil précalculé en tâche de fond, arbre et séries en cache, gzip,
  limite de 20 req/s par IP, lectures lourdes mises en file.

Banc sur 1 cœur saturé par 8 clients web, cibles en boucle locale (vraie
latence ≈ 0,03 ms) :

| Sous charge web | Avant | Sonde isolée |
|---|---|---|
| Médiane mesurée | 1,708 ms | **0,016 ms** |
| Pic mesuré | 50,5 ms | **0,041 ms** |
| API, p95 | 9–12 ms | 9–14 ms |

Détails et mode intégré (`--embedded`) : [DEPLOY.md, section 10](../DEPLOY.md#10-probe-isolation-and-performance).

## Ce qui tourne

| Boucle | Période | Rôle |
|---|---|---|
| ordonnanceur | 1 s | déclenche les passes dont l'offset correspond |
| rollups | 30 s | cascade samples → 1 min → 5 min → 1 h → 1 j |
| archive | 30 s | scellement horaire, upload S3, rotation par quota |
| purge | 6 h | applique la rétention par palier |

Les offsets de départ sont dérivés d'un hash de l'identifiant de cible :
les passes s'étalent sur l'intervalle au lieu de partir toutes à la
seconde ronde.

## Percentiles fusionnables

Chaque passe produit un DDSketch (histogramme à buckets logarithmiques,
γ = 1,02, erreur relative garantie à 1 %) sérialisé en 60 à 150 octets.
La fusion est une addition bucket à bucket, donc associative et exacte :
le p95 horaire calculé depuis 60 sketches d'une minute vaut le p95 des
échantillons bruts. Une moyenne de p95, elle, ne veut rien dire — c'est
l'erreur classique des outils de supervision.

`go test ./...` vérifie cette propriété sur 1 200 échantillons.

## Résolution automatique

L'API choisit la table selon la fenêtre demandée, le client ne connaît
que `from` et `to` :

| Fenêtre | Table | Pas |
|---|---|---|
| ≤ 6 h | `samples` | la passe elle-même |
| ≤ 4 j | `roll_1m` | 1 min |
| ≤ 45 j | `roll_5m` | 5 min |
| ≤ 500 j | `roll_1h` | 1 h |
| au-delà | `roll_1d` | 1 j |

Le zoom à la souris rappelle simplement `/api/v1/series` avec la nouvelle
plage ; la granularité suit toute seule.

## Stockage

Trois modes, dans `settings.storage`, modifiables à chaud par l'API.

- `s3` — les chunks horaires partent sur S3 et sont purgés localement.
- `local` — tout reste sur disque, borné par `quota_bytes` et par les
  seuils haut/bas.
- `hybrid` — local pendant `keep_local_hours`, puis S3, avec rotation.

La rotation évince d'abord les chunks déjà présents sur S3 (récupérables),
puis les plus anciens, jusqu'à repasser sous le seuil bas. Les agrégats
horaires et journaliers ne sont jamais évincés : ils pèsent quelques
centaines de mégaoctets pour dix ans.

L'upload S3 est signé en SigV4 à la main (≈ 70 lignes) plutôt que via un
SDK, ce qui garde l'arbre de dépendances à un seul module. Compatible
Scaleway, OVH, Backblaze, Cloudflare R2, MinIO et Ceph (`path_style`).

## API

Public :

```
GET  /api/v1/tree
GET  /api/v1/series?target=42&from=now-3h&to=now
GET  /api/v1/charts?from=now-24h
GET  /api/v1/events?from=now-7d
GET  /api/v1/live                      (SSE)
GET  /healthz
```

`from` et `to` acceptent un epoch, une date RFC3339 ou une expression
relative (`now-3h`, `now-45d`).

Administration, avec `Authorization: Bearer <admin_token>` :

```
GET    /api/v1/admin/storage
PUT    /api/v1/admin/storage
GET    /api/v1/admin/targets
POST   /api/v1/admin/targets
DELETE /api/v1/admin/targets/{id}
POST   /api/v1/admin/categories
POST   /api/v1/admin/events
```

Ingestion depuis une sonde distante (`Authorization: Bearer <jeton de sonde>`) :

```
POST /api/v1/ingest
{"probe":"par-01","seq":42,"batch":[
  {"target_id":7,"ts":1757768460,"sent":20,"lost":0,"rtt_us":[8412,8390]}
]}
```

Exemples :

```bash
TOKEN=$(sudo jq -r .admin_token /etc/smokestack/config.json)

curl -s localhost:8080/api/v1/tree | jq

curl -s -X POST localhost:8080/api/v1/admin/categories \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"slug":"transit","menu_fr":"Transitaires","menu_en":"Transit"}'

curl -s -X POST localhost:8080/api/v1/admin/targets \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"category_id":2,"title":"Cogent Paris","host":"38.0.0.1",
       "interval_s":30,"packets":10,"spacing_ms":200,"timeout_ms":1500}'

curl -s -X PUT localhost:8080/api/v1/admin/storage \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"mode":"hybrid",
       "s3":{"endpoint":"s3.fr-par.scw.cloud","bucket":"smoke-prod",
             "prefix":"par-01","region":"fr-par",
             "access_key":"SCW...","secret_key":"...","path_style":false},
       "local":{"quota_bytes":21474836480,"high_watermark":0.85,
                "low_watermark":0.70,"keep_local_hours":48}}'

curl -s localhost:8080/api/v1/admin/storage -H "Authorization: Bearer $TOKEN" | jq .report
```

Le secret S3 ne ressort jamais de l'API ; un `PUT` sans `secret_key`
conserve celui déjà en base.

## Garde-fous

La création de cible refuse une rafale qui ne tient pas dans son
intervalle : `packets × spacing_ms + timeout_ms` doit rester sous 75 %
de `interval_s`. À 30 secondes, cela impose 10 paquets espacés de 200 ms
plutôt que les 20 paquets par seconde de SmokePing.

## Limites actuelles

- IPv6 non câblé (le schéma le prévoit, le prober ouvre un socket `ip4`).
- Archive en NDJSON gzip, pas encore en Parquet.
- Pas de traceroute déclenché sur anomalie.
- Back-office en français uniquement ; les pages publiques sont traduites.
- Édition d'une cible existante et gestion des sondes distantes à venir.

## Page éditeur

`GET /api/v1/site` expose les métadonnées de l'instance, rendues par
`/about.html` en FR/EN. Le téléphone du NOC n'apparaît jamais sur la page
publique : il n'est renvoyé qu'aux appels authentifiés.

```bash
curl -s -X PUT localhost:8080/api/v1/admin/site \
  -H "Authorization: Bearer $TOKEN" -d '{
    "title":"Latence — Exemple Télécom",
    "org":"Exemple Télécom SAS",
    "asn":"AS64500",
    "owner":"NOC Exemple Télécom",
    "email":"contact@example.net",
    "noc_email":"noc@example.net",
    "noc_phone":"+33 1 00 00 00 00",
    "url":"https://www.example.net",
    "peeringdb":"https://www.peeringdb.com/asn/64500",
    "location":"Paris, FR",
    "timezone":"Europe/Paris",
    "description":"Mesures depuis notre réseau vers des destinations publiques.",
    "legal":"Éditeur : Exemple Télécom SAS — hébergement propre."
  }'
```

## Fédération

Composant `federation`, désactivé par défaut. Chaque instance possède une
paire de clés Ed25519 (`/var/lib/smokestack/fed.key`, 0600) et une empreinte
courte que deux opérateurs comparent hors bande avant d'approuver.

Les requêtes inter-instances sont signées **au niveau du message**, pas du
transport : en-têtes `X-Fed-ASN`, `X-Fed-Timestamp`, `X-Fed-Nonce`,
`X-Fed-Signature`, sur `MÉTHODE \n CHEMIN \n TS \n NONCE \n sha256(corps)`.
Tolérance d'horloge de 5 minutes, cache de nonces contre le rejeu. Ça passe
derrière n'importe quel reverse proxy sans plomberie de certificats.

```json
"federation": {
  "enabled": true,
  "asn": "AS64500",
  "org": "Exemple Télécom SAS",
  "base_url": "https://latence.example.net",
  "anchors": ["93.29.0.1", "2a01:xxxx::1"]
}
```

Appairage :

```bash
curl -s localhost:8080/api/v1/admin/fed/identity \
  -H "Authorization: Bearer $TOKEN" | jq -r .fingerprint

curl -s -X POST localhost:8080/api/v1/admin/fed/peers \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"url":"https://latence.autre-operateur.net"}'
# → compare l'empreinte retournée avec celle communiquée par l'opérateur

curl -s -X POST localhost:8080/api/v1/admin/fed/peers/2/trust \
  -H "Authorization: Bearer $TOKEN"
```

Approuver un pair crée automatiquement les cibles de mesure vers ses ancres,
dans une catégorie « Fédération ». Toutes les cinq minutes, chaque instance
pousse aux autres ses mesures vers leurs ancres : la matrice inter-AS se
construit d'elle-même (`GET /api/v1/fed/matrix`).

### Alerting NOC

Quand une ancre fédérée dépasse 5 % de perte sur dix minutes, un incident
est ouvert et diffusé aux pairs pour corroboration. La notification sortante
n'est envoyée que si **quatre conditions** sont réunies :

1. au moins 3 observateurs indépendants ont corroboré ;
2. le préavis de 5 minutes est écoulé ;
3. l'AS mis en cause n'a pas acquitté via `/api/v1/fed/ack` ;
4. l'AS mis en cause est un pair approuvé, qui a donc consenti en
   rejoignant — aucun mail non sollicité vers un AS non membre.

Fenêtre de suppression de 6 heures par couple (AS, cible). Le message est
un constat corroboré, pas un diagnostic, et le dit explicitement.

```bash
curl -s -X PUT localhost:8080/api/v1/admin/fed/notify \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"enabled":true,"smtp_host":"smtp.example.net","smtp_port":587,
       "smtp_user":"noc@example.net","smtp_pass":"...",
       "from":"noc@example.net","webhook_url":"https://mattermost/hooks/xxx"}'
```

### Ce qui n'est pas implémenté

La mesure à la demande vers un préfixe tiers est volontairement absente :
c'est le vecteur d'amplification et de reconnaissance. Si elle arrive, elle
sera restreinte aux préfixes dont le demandeur prouve l'origine par ROA.
La localisation du saut fautif par intersection de traceroutes n'est pas
là non plus — il faut d'abord que la matrice tourne en production.

## Back-office web

`https://votre-instance/admin`

Au premier accès, aucun compte n'existe : l'écran d'amorçage crée le compte
**master**. La fenêtre se referme dès qu'un compte existe — l'endpoint
`/api/v1/auth/setup` renvoie 409 ensuite.

### Rôles

| Rôle | Périmètre |
|---|---|
| `master` | tout, y compris la gestion des comptes |
| `admin` | configuration, fédération, stockage, page éditeur — pas les comptes |
| `editor` | cibles et catégories seulement |
| `viewer` | lecture seule |

Il doit toujours rester au moins un master actif : le dernier ne peut être ni
supprimé, ni rétrogradé, ni désactivé.

### Sécurité de la session

- Mots de passe en PBKDF2-HMAC-SHA256, 210 000 itérations, sel de 16 octets,
  implémenté en stdlib pour ne pas ajouter de dépendance.
- Cookie de session `HttpOnly`, `SameSite=Lax`, `Secure` dès que la requête
  arrive en HTTPS (directement ou via `X-Forwarded-Proto`). Jeton de 32 octets
  stocké haché en base, durée de 12 heures.
- Jeton CSRF exigé sur toute méthode mutante en mode cookie.
- 8 tentatives de connexion par IP et par tranche de 10 minutes, message
  d'erreur identique que le compte existe ou non.
- Un changement de mot de passe ou une désactivation ferme toutes les sessions
  du compte.
- Le jeton `admin_token` de `config.json` continue de fonctionner en `Bearer`
  pour l'automatisation, et vaut `master`. Le CSRF ne s'applique pas à ce mode.
- Journal d'audit : connexions, échecs, créations, décisions d'appairage.

## Appairage en deux temps

Chaque instance expose une page publique `/pairing` qui affiche son AS, son
organisation, son empreinte de clé et ses ancres. C'est l'URL à communiquer.

```
A                                    B
│  ajoute https://B/pairing
│  GET  B/api/v1/fed/profile         → lit la clé et l'empreinte de B
│  POST B/api/v1/fed/pairing/request → demande signée avec la clé de A
│                                      B enregistre "in / pending"
│                                      202, rien n'est approuvé
│                                    ┌ l'admin de B voit la demande
│                                    │ compare l'empreinte hors bande
│                                    └ accepte → B approuve A
│  POST A/api/v1/fed/pairing/response ← réponse signée avec la clé de B
│  A vérifie la signature contre la clé lue à l'étape 1
│  A approuve B                        appairage établi des deux côtés
```

Ce que ça garantit : la signature de la demande prouve la possession de la clé
annoncée, pas l'identité. C'est la comparaison d'empreinte par un humain qui
fait la confiance, et l'accord est requis **des deux côtés** — A a choisi
d'envoyer, B a choisi d'accepter. La réponse doit être signée par la clé que A
a lue dans le profil public au départ, ce qui empêche un tiers de répondre à sa
place. Le jeton d'appairage corrèle demande et réponse et interdit le rejeu.

Accepter une demande approuve le pair et crée automatiquement les cibles de
mesure vers ses ancres, dans une catégorie « Fédération ».

### En ligne de commande

```bash
# notre empreinte, à communiquer à l'autre opérateur
curl -s localhost:8080/api/v1/admin/fed/identity \
  -H "Authorization: Bearer $TOKEN" | jq -r .fingerprint

# demander un appairage
curl -s -X POST localhost:8080/api/v1/admin/fed/pairing \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"url":"https://smokestack.autre.fr/pairing","message":"AS64500 souhaite échanger"}'

# lister les demandes, puis décider
curl -s localhost:8080/api/v1/admin/fed/pairing -H "Authorization: Bearer $TOKEN" | jq
curl -s -X POST localhost:8080/api/v1/admin/fed/pairing/3/decide \
  -H "Authorization: Bearer $TOKEN" -d '{"accept":true}'
```

### À faire avant d'exposer sur Internet

Mettre un reverse proxy en TLS devant — le cookie ne passe en `Secure` qu'en
HTTPS. Uniquement `proxy_pass`, jamais de `root` ni d'`alias` : le binaire ne
sert aucun fichier depuis le disque, et c'est ce qui garantit que `config.db`,
`metrics.db` et `fed.key` restent inatteignables par une URL.

## Langues

Chaque langue est un fichier JSON dans `web/i18n/` (embarqué dans le binaire).
**L'anglais (`en.json`) est la référence obligatoire** : sans lui, smokestack
refuse de démarrer. Toute clé absente d'une autre langue s'affiche en anglais.

Livrées : `en` (référence), `fr`, `de`, `es` — 162 clés, toutes complètes.

Ajouter ou corriger une langue sans recompiler :

```sh
cp web/i18n/en.json /var/lib/smokestack/i18n/it.json   # puis traduire
# back-office → Langues → Recharger
```

Les fichiers acceptent des objets imbriqués (`{"home":{"faults":"…"}}` → `home.faults`)
et des variables (`{n}`, `{total}`). Au chargement, chaque langue est comparée à
l'anglais : clés manquantes, clés inconnues et **variables divergentes** sont
signalées dans le back-office. Le test `TestI18nFiles` échoue si une langue
livrée est incomplète.

Côté navigateur, `i18n.js` choisit la langue ainsi : `?lang=`, choix mémorisé,
langues du navigateur, langue par défaut de l'instance (page éditeur), anglais.

## Appairage — justification, note privée, affichage public

Une demande d'appairage porte :

| Champ | Obligatoire | Vu par |
|---|---|---|
| Justification (≥ 20 caractères) | oui | l'administrateur distant |
| Note privée | non | l'administrateur distant |
| Contact signataire | non | l'administrateur distant |
| Accord d'affichage public | — | les deux côtés |

La réponse (acceptation ou refus) peut porter une **réponse privée**, visible
seulement par le demandeur. Aucun de ces textes n'apparaît sur une page publique.

**Affichage public : double consentement.** Un appairage n'est listé sur
`/federation` que si les deux administrateurs l'ont accepté. Chacun peut le
masquer à tout moment ; aucun ne peut l'afficher sans l'accord de l'autre
(`PATCH /api/v1/admin/fed/peers/{id}` refuse `public:true` sans consentement).

## Réseau hôte (RIPEstat + PeeringDB)

La page `/network` décrit l'AS qui héberge l'instance :

- **RIPEstat** (routage observé) : titulaire, pays, préfixes v4/v6 annoncés,
  nombre de voisins amont/aval, principaux voisins amont ;
- **PeeringDB** (déclaratif) : type, politique de peering, trafic, ratio,
  as-set IRR, présence aux points d'échange, datacenters.

Cache de 24 h en base, actualisation en tâche de fond. Le service ne répond que
pour **notre AS et nos pairs approuvés** : l'instance n'est pas un proxy ouvert.
Une clé PeeringDB facultative (`"peeringdb_api_key"` dans `config.json`)
relève les limites de requêtes anonymes.

```sh
curl https://smokestack.exemple.fr/api/v1/asn          # notre AS
curl https://smokestack.exemple.fr/api/v1/asn/64501    # un pair approuvé
```

## Pages publiques

| Page | Contenu |
|---|---|
| `/` | graphes (refonte sur la maquette v3 à venir) |
| `/federation` | pairs publics, latence dans les deux sens, matrice inter-AS |
| `/network` | réseau hôte, RIPEstat + PeeringDB |
| `/pairing` | identité, empreinte, marche à suivre |
| `/about` | exploitant, contacts, méthode |

Toutes partagent `app.css` (thème sombre automatique), `i18n.js` et `app.js`.
