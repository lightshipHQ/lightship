-- Marked fields carry the logical type used by the shared policy/filter expression compiler.
-- Existing rows were string-only, so the default is their exact previous meaning.
alter table field
  add column logical_type text not null default 'string'
  check (logical_type in ('string', 'string_array', 'boolean', 'number'));
