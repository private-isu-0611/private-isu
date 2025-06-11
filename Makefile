ALP_BIN:=alp
COMPETITION_IP:=35.78.68.97

.PHONY: help bench cat-nginx-access vim-nginx-conf cat-nginx-sites vim-nginx-sites install-homebrew install-alp alp-json show-logs mysql mysql-desc show-slow-query restart-go restart-nginx restart-mysql restart-all

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'

bench: ## Run benchmarker
	curl https://xnvvb925bl.execute-api.ap-northeast-1.amazonaws.com/

# nginx
cat-nginx-access: ## Show nginx access log
	sudo cat /var/log/nginx/access.log

vim-nginx-conf: ## Edit nginx config
	sudo vim /etc/nginx/nginx.conf

cat-nginx-sites: ## Show nginx sites config
	sudo cat /etc/nginx/sites-available/isucon.conf
	
vim-nginx-sites: ## Edit nginx sites config
	sudo vim /etc/nginx/sites-available/isucon.conf

# alp
# Homebrew
install-homebrew: ## Install Homebrew
	/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"

install-alp: ## Install alp via Homebrew
	brew install alp

alp-json: ## Run alp analysis on nginx logs
	sudo cat /var/log/nginx/access.log | alp json \
		--sort sum -r \
		-m "posts/[0-9+], /@\w+, /image/\d+" \
		-o count,method,uri,min,avg,max,sum

# Logs
show-logs: ## Show service logs
	sudo journalctl -u isu-go.service -f

# MySQL	
mysql: ## Connect to MySQL as root
	sudo mysql

mysql-desc: ## Connect to MySQL as isucon user
	sudo mysql -u isucon -p -h 127.0.0.1 isucon

show-slow-query: ## Show MySQL slow query log
	sudo mysqldumpslow /var/log/mysql/mysql-slow.log

# Restart Service
restart-go: ## Restart Go service
	sudo systemctl restart isu-go.service

restart-nginx: ## Restart nginx service
	sudo systemctl restart nginx.service

restart-mysql: ## Restart MySQL service
	sudo systemctl restart mysql.service

restart-all: ## Restart all services
	sudo systemctl restart isu-go.service
	sudo systemctl restart nginx.service
	sudo systemctl restart mysql.service
