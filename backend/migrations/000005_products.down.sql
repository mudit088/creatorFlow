-- product_files first: it carries the foreign key, so dropping products before
-- it would fail on the dependency.
DROP TABLE IF EXISTS product_files;
DROP TABLE IF EXISTS products;
