#!/bin/sh
set -e -u

[ ! $# -eq 1 ] && { echo "usage: $0 <binfile>"; exit 1; }

F=`basename $1`
NF=`echo $F| sed "s/[^[:alnum:]]/_/g"`
[ ! -r $F ] && { echo "cannot read $F"; exit 1; }

UTIL="xxd"
which $UTIL > /dev/null 2>&1 || { echo "cannot find $UTIL"; exit 1; }

X=`which $UTIL`
echo "var _bin_$NF = []uint8{"
$X -i $F |tail -n+2 | sed "s/ \(0x[[:xdigit:]][[:xdigit:]]\)$/\1,/" \
	|sed "s/^unsigned int.*\(=.*\);/var _bin_${NF}_len int \1/"
