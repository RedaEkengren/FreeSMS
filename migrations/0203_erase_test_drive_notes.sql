-- rollback: safe
-- Two column grants; the previous release never updates either.
--
-- A person erased (ErasePerson) loses what was written about them on a test
-- drive (#173): the note on their licence check, and what was noted as the
-- keys went out or came back. That the drive and the check happened stays.
GRANT UPDATE (note) ON test_drive_licence_checks TO freesms_app;
GRANT UPDATE (note) ON test_drive_events TO freesms_app;
