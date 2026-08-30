-- Recipes have one name; repository-backed API views derive owner/repository from source.
ALTER TABLE recipes DROP COLUMN display_name;
