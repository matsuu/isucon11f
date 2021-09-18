#!/bin/sh
set -ex

(
  cd /home/isucon/webapp/go
  make
)

# sudo rsync -av --delete isu2:/home/isucon/webapp/ /home/isucon/webapp/
# sudo rsync -av --delete isu2:/home/isucon/env.sh /home/isucon/env.sh
# sudo rsync -av --delete isu2:/etc/mysql/ /etc/mysql/

# mysql
sudo mysql -e "TRUNCATE TABLE performance_schema.events_statements_summary_by_digest"
#sudo rm -f /var/log/mysql/mysql-slow.log
#sudo mysqladmin flush-slow-log

# app
sudo systemctl restart isucholar.go

# nginx
sudo rm -f /var/log/nginx/access.log
sudo systemctl reload nginx

echo "OK"
