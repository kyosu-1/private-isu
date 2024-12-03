all: app

NGINX_LOG = /var/log/nginx/access.log
DB_SLOW_LOG = /var/log/mysql/mysql-slow.log
IMAGE_DIR = /home/isucon/private_isu/webapp/public/image

app: *.go go.mod go.sum
	go build -o app

deploy:
	go build -o app
	sudo systemctl restart nginx
	sudo systemctl restart isu-go

clean-logs:
	sudo truncate -s 0 $(NGINX_LOG)
	sudo truncate -s 0 $(DB_SLOW_LOG)
	sudo rm -rf $(IMAGE_DIR)/*
	mkdir -p $(IMAGE_DIR)
