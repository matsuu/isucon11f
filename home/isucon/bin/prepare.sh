#!/bin/sh
set -ex

(
  cd /home/isucon/webapp/go
  make
)

for s in isu2 isu3 ; do
  sudo rsync -av --delete /home/isucon/bin/ $s:/home/isucon/bin/
  sudo rsync -av --delete --exclude "assignments/" /home/isucon/webapp/ $s:/home/isucon/webapp/
  sudo rsync -av --delete /home/isucon/env.sh $s:/home/isucon/env.sh
  sudo rsync -av --delete /etc/mysql/ $s:/etc/mysql/
done

# mysql
#sudo ssh isu3 mysql -e "TRUNCATE TABLE performance_schema.events_statements_summary_by_digest"
sudo ssh isu3 rm -f /var/log/mysql/mysql-slow.log
sudo ssh isu3 mysqladmin flush-logs

# app
sudo systemctl restart isucholar.go

# nginx
sudo rm -f /var/log/nginx/access.log
sudo systemctl reload nginx

echo "OK"
