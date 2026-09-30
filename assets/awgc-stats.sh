#!/bin/bash
# Copyright (c) 2026 AACH
# SPDX-License-Identifier: MIT
# awgc-stats.sh — накопление статистики трафика клиентов AmneziaWG по дням.
# Ставится программой AmneziaWG Cascade, запускается cron раз в 5 минут.
# Счётчики awg обнуляются при перезапуске интерфейса, поэтому здесь хранятся
# последние значения и в дневной итог добавляется только прирост.
# Файл /root/awg/awgc-stats/daily.tsv: дата<TAB>клиент<TAB>получено_от_клиента<TAB>отправлено_клиенту (байты)
set -u
D=/root/awg/awgc-stats
CONF=/etc/amnezia/amneziawg/awg0.conf
mkdir -p "$D"
exec 9>"$D/.lock"
flock -w 60 9 || exit 0
touch "$D/state" "$D/daily.tsv"
DUMP=$(timeout 10 awg show awg0 dump 2>/dev/null) || exit 0
TODAY=$(date +%F)
CUTOFF=$(date -d '-400 days' +%F 2>/dev/null || echo 0000-00-00)

awk '/^#_Name = /{n=substr($0,10); next} /^PublicKey[ \t]*=/{if(n!=""){sub(/^PublicKey[ \t]*=[ \t]*/,""); print $0"\t"n; n=""}}' "$CONF" > "$D/names.tmp"
printf '%s\n' "$DUMP" | tail -n +2 | awk -F'\t' 'NF>=7{print $1"\t"$6"\t"$7}' > "$D/cur.tmp"

awk -F'\t' -v OFS='\t' -v today="$TODAY" -v cutoff="$CUTOFF" \
    -v st="$D/state.new" -v dl="$D/daily.new" '
FILENAME==ARGV[1] { name[$1]=$2; next }
FILENAME==ARGV[2] { lrx[$1]=$2; ltx[$1]=$3; next }
FILENAME==ARGV[3] { if ($1 < cutoff) next; k=$1 OFS $2; if (!(k in drx)) ord[++n]=k; drx[k]=$3; dtx[k]=$4; next }
{
  pk=$1; rx=$2+0; tx=$3+0
  ns[pk]=rx OFS tx
  c=name[pk]; if (c=="") next
  a = (pk in lrx && rx >= lrx[pk]) ? rx-lrx[pk] : rx
  b = (pk in ltx && tx >= ltx[pk]) ? tx-ltx[pk] : tx
  if (a==0 && b==0) next
  k=today OFS c; if (!(k in drx)) { ord[++n]=k; drx[k]=0; dtx[k]=0 }
  drx[k]+=a; dtx[k]+=b
}
END {
  for (p in ns) print p, ns[p] > st
  for (i=1;i<=n;i++) print ord[i], drx[ord[i]], dtx[ord[i]] > dl
}' "$D/names.tmp" "$D/state" "$D/daily.tsv" "$D/cur.tmp"

touch "$D/state.new" "$D/daily.new"
mv -f "$D/state.new" "$D/state"
mv -f "$D/daily.new" "$D/daily.tsv"
rm -f "$D/names.tmp" "$D/cur.tmp"
