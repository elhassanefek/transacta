COMPOSE := docker compose -f docker/docker-compose.yml

.PHONY: up up-db down migrate-up migrate-down migrate-down-one migrate-create migrate-version psql

## Start the full stack: Postgres, automatic migrations, then the API.
## Requires JWT_SECRET set (e.g. in a .env file at the repo root -- see
## .env.example).
up:
	$(COMPOSE) up -d

## Start Postgres only, without migrating or starting the API -- for
## running `go test` integration suites against a bare local database.
up-db:
	$(COMPOSE) up -d postgres

## Stop everything
down:
	$(COMPOSE) down

## Apply all pending migrations
migrate-up:
	$(COMPOSE) run --rm migrate up

## Roll back ALL migrations (careful — asks nothing, just does it)
migrate-down:
	$(COMPOSE) run --rm migrate down -all

## Roll back exactly one migration
migrate-down-one:
	$(COMPOSE) run --rm migrate down 1

## Show current migration version (and whether it's dirty)
migrate-version:
	$(COMPOSE) run --rm migrate version

## Scaffold a new migration pair: make migrate-create name=add_users_table
migrate-create:
	$(COMPOSE) run --rm --entrypoint migrate migrate \
		create -ext sql -dir /migrations -seq $(name)

## Quick psql shell into the running container
psql:
	docker exec -it transacta_db psql -U transacta -d transacta