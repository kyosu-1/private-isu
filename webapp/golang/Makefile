all: app

app: *.go go.mod go.sum
	go build -o app

deploy:
	go build -o app
	sudo systemctl restart nginx
	sudo systemctl restart app