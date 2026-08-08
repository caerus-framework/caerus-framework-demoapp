-- Tear down in reverse FK order. Used by golang-migrate down; demoapp rarely
-- needs this locally, but keeping down.sql honest is part of the migrate story.
DROP TABLE IF EXISTS prices;
DROP TABLE IF EXISTS vehicles;
DROP TABLE IF EXISTS lots;
