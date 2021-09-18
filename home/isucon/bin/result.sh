#!/bin/sh

set -x

rm -f /result/*.* 
sudo cat /var/log/nginx/access.log | ${HOME}/go/bin/kataribe -f ${HOME}/kataribe.toml > /result/kataribe.log
sudo ${HOME}/go/bin/slowquery2tsv -u isucon -p isucon > /result/slowquery.tsv
#sudo cat /var/log/mysql/mysql-slow.log | pt-query-digest --limit 100% > /result/slowquery.log
sudo perl ${HOME}/bin/mysqltuner.pl > /result/mysqltuner.log

sudo git add /
sudo git commit -av
sudo git push origin main
