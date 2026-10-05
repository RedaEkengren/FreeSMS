// Reads a SIE file with jsiSIE and prints what it understood.
//
// Not a validator of our own: the point is that the reading is somebody
// else's. Every exception jsiSIE records is printed, and the facts it parsed
// -- company, accounts, vouchers, rows -- are printed as it holds them, so the
// Go side can compare them with what the file was meant to say.
using System;
using System.Collections.Generic;
using System.Globalization;
using System.Linq;
using System.Text.Json;
using jsiSIE;

if (args.Length != 1)
{
    Console.Error.WriteLine("usage: siecheck <file.se>");
    return 2;
}

var doc = new SieDocument { ThrowErrors = false };
// A stream, not the file name: given a name, jsiSIE opens the file for
// writing as well, which fails on a read-only mount and has no business
// succeeding anywhere else.
using (var stream = System.IO.File.OpenRead(args[0]))
{
    doc.ReadDocument(stream);
}

var report = new
{
    errors = (doc.ValidationExceptions ?? new List<Exception>()).Select(e => e.Message).ToList(),
    program = doc.PROGRAM,
    format = doc.FORMAT,
    sietyp = doc.SIETYP,
    company = doc.FNAMN?.Name,
    accounts = (doc.KONTO ?? new Dictionary<string, SieAccount>())
        .ToDictionary(k => k.Key, k => k.Value.Name),
    vouchers = (doc.VER ?? new List<SieVoucher>()).Select(v => new
    {
        series = v.Series,
        number = v.Number,
        date = v.VoucherDate.ToString("yyyy-MM-dd", CultureInfo.InvariantCulture),
        text = v.Text,
        rows = v.Rows.Select(r => new
        {
            account = r.Account?.Number,
            amount = r.Amount.ToString("0.00", CultureInfo.InvariantCulture),
        }).ToList(),
    }).ToList(),
};
Console.WriteLine(JsonSerializer.Serialize(report));
return 0;
