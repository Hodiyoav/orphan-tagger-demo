.PHONY: up status create delete aws-tags logs down

up:       ## build the lab (kind + fake AWS + Crossplane + Kyverno + orphan-tagger)
	./scripts/up.sh

status:   ## show what `make up` installed
	./scripts/status.sh

create:   ## command 1: create a taggable + a non-taggable AWS resource
	./scripts/create.sh

delete:   ## command 2: delete them and watch the orphan-tagger work
	./scripts/delete.sh

aws-tags: ## inspect the fake AWS
	./scripts/aws-tags.sh

logs:     ## follow the orphan-tagger watcher
	@. scripts/env.sh && k -n $$NS logs -f deploy/orphan-tagger-watcher

down:     ## remove everything
	./scripts/down.sh
