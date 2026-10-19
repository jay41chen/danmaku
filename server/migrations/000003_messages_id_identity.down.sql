BEGIN;

-- 把identity的改動丟掉，改回int
ALTER TABLE messages 
    ALTER COLUMN id DROP IDENTITY,
    ALTER COLUMN id TYPE INT;

-- 加回sequence
CREATE SEQUENCE messages_id_seq AS INT OWNED BY messages.id;
-- 重設sequence
ALTER TABLE messages
    ALTER COLUMN id SET DEFAULT nextval('messages_id_seq');

-- 接回原本的MAX_VALUE
SELECT setval(pg_get_serial_sequence('messages','id'), COALESCE(MAX(id), 0) + 1, false)
FROM messages;

COMMIT;