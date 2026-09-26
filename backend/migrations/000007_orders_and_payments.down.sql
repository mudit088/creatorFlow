-- Reverse dependency order: every table holding a foreign key into orders goes
-- first, or the DROP fails on the dependency.
DROP TABLE IF EXISTS entitlements;
DROP TABLE IF EXISTS payment_webhooks;
DROP TABLE IF EXISTS payments;
DROP TABLE IF EXISTS order_items;
DROP TABLE IF EXISTS orders;
