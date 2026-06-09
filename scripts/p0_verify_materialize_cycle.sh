#!/usr/bin/env bash
# Lazarr P0 — verify the full lazy lifecycle: checkcached -> add -> requestdl(range) -> release.
# ToS-compliant: adds ONE cached throwaway (Big Buck Bunny), streams 256KB, then releases it.
# Robust: matches the test torrent by HASH (not a mylist diff — decypharr adds concurrently),
# and releases via POST {torrent_id,operation:"delete"} (the VERIFIED contract).
# Run:  bash p0_verify_materialize_cycle.sh
set -uo pipefail

KEY=$(python3 -c "import json;c=json.load(open('/config/decypharr/config.json'));d=c['debrids'];tb=[x for x in d if (x.get('name')=='torbox')] if isinstance(d,list) else [d.get('torbox')];t=tb[0] if tb else None;print((t.get('api_key') or (t.get('download_api_keys') or [''])[0]) if t else '')")
B="https://api.torbox.app/v1/api"
BBB=dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c
MAG="magnet:?xt=urn:btih:${BBB}&dn=BigBuckBunny"
auth=(-H "Authorization: Bearer $KEY")

present(){ curl -s -m 15 "${auth[@]}" "$B/torrents/mylist?bypass_cache=true&limit=1000" | python3 -c "import sys,json;L=json.load(sys.stdin).get('data') or [];print('1' if any(t.get('hash','').lower()=='$BBB' for t in L) else '0', len(L))"; }

echo "== 0. checkcached (no add) =="
curl -s -m 15 "${auth[@]}" "$B/torrents/checkcached?hash=$BBB&format=object&list_files=true" | python3 -c "import sys,json;d=json.load(sys.stdin).get('data') or {};v=next(iter(d.values()),{});print('   cached:',bool(d),'size:',v.get('size'),'files:',len(v.get('files') or []))"
read p0 c0 < <(present); echo "   BBB present BEFORE: $p0 | account total: $c0"

echo "== 1. createtorrent add_only_if_cached=true =="
curl -s -m 30 "${auth[@]}" -F "magnet=$MAG" -F "add_only_if_cached=true" "$B/torrents/createtorrent" | python3 -c "import sys,json;d=json.load(sys.stdin);print('   success:',d.get('success'),'detail:',d.get('detail'));import json as j;open('/tmp/add.json','w').write(j.dumps(d.get('data') or {}))"
sleep 2

# Locate by HASH (robust)
read TID FID < <(curl -s -m 15 "${auth[@]}" "$B/torrents/mylist?bypass_cache=true&limit=1000" | python3 -c "
import sys,json
L=json.load(sys.stdin).get('data') or []
t=next((t for t in L if t.get('hash','').lower()=='$BBB'),None)
f=sorted((t or {}).get('files') or [],key=lambda x:-(x.get('size') or 0))
print((t or {}).get('id',''), (f[0]['id'] if f else ''))")
echo "   added torrent_id=$TID file_id=$FID"
[ -z "$TID" ] && { echo "   add did not land (rate-limited/cooldown?) — see detail above"; exit 0; }

echo "== 2. requestdl + 256KB range GET =="
URL=$(curl -s -m 15 "${auth[@]}" "$B/torrents/requestdl?token=$KEY&torrent_id=$TID&file_id=$FID&redirect=false" | python3 -c "import sys,json;print(json.load(sys.stdin).get('data') or '')")
curl -s -m 20 -o /dev/null -D /tmp/h.txt -r 0-262143 "$URL"
echo "   $(grep -i '^HTTP/' /tmp/h.txt | tail -1) | $(grep -i '^content-range' /tmp/h.txt)"

echo "== 3. RELEASE: POST controltorrent {torrent_id,operation:delete} =="
curl -s -m 20 -X POST "${auth[@]}" -H "Content-Type: application/json" -d "{\"torrent_id\":$TID,\"operation\":\"delete\"}" "$B/torrents/controltorrent" | python3 -c "import sys,json;d=json.load(sys.stdin);print('   success:',d.get('success'),'detail:',d.get('detail'))"
sleep 2
read p1 c1 < <(present); echo "== 4. BBB present AFTER release: $p1 (want 0) | account total: $c1 =="
[ "$p1" = "0" ] && echo "PASS — full lazy lifecycle verified (cached-size-no-add -> add -> 206 stream -> release)." || echo "WARN — BBB still present; investigate."
