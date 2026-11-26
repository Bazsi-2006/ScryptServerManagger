-- The general BACKUPS permission (bit 16) is split into BACKUP_CREATE (64)
-- and BACKUP_DOWNLOAD (128). Everyone who held BACKUPS keeps exactly the
-- capabilities it gave: both new flags. The other bits keep their meaning.
-- Rows without bit 16 are untouched, so running this again changes nothing.
UPDATE server_permissions
   SET mask = (mask & ~16) | 64 | 128
 WHERE (mask & 16) != 0;
