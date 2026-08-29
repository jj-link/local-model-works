-- Per-member fabric wiring replaces the original first-member/global fields.
ALTER TABLE fabrics ADD COLUMN bindings TEXT NOT NULL DEFAULT '[]';

-- Preserve existing fabrics as editable bindings. Operators can correct
-- asymmetric interface/RDMA names and add a GID index through the UI.
UPDATE fabrics
SET bindings = COALESCE((
    SELECT json_group_array(json_object(
        'node_id', member.value,
        'interface_name', COALESCE(fabrics.interface_name, ''),
        'address', CASE WHEN member.key = 0 THEN COALESCE(fabrics.address, '') ELSE '' END,
        'rdma_device', COALESCE(fabrics.rdma_device, '')
    ))
    FROM json_each(fabrics.members) AS member
), '[]')
WHERE bindings = '[]';
