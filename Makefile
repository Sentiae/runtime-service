.PHONY: migrate-up migrate-down

DB_URL ?= postgres://postgres:postgres@localhost:5432/runtime_service?sslmode=disable

migrate-up:
	migrate -path ./migrations -database "$(DB_URL)" up

migrate-down:
	migrate -path ./migrations -database "$(DB_URL)" down
