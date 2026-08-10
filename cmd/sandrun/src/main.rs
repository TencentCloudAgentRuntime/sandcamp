use std::env;
use std::ffi::OsString;

fn main() {
    let mut process_arguments = env::args_os();
    let _ = process_arguments.next();
    let arguments = process_arguments.collect::<Vec<OsString>>();
    if arguments.len() == 1 && sandrun::is_help(&arguments[0]) {
        print!("{}", sandrun::USAGE);
        return;
    }
    if arguments.len() == 1 && sandrun::is_version(&arguments[0]) {
        println!("sandrun {}", env!("CARGO_PKG_VERSION"));
        return;
    }

    let config = match sandrun::parse_args(arguments) {
        Ok(config) => config,
        Err(error) => {
            eprintln!("sandrun: invalid_arguments: {error}");
            eprintln!("Try 'sandrun --help' for usage.");
            std::process::exit(2);
        }
    };
    if let Err(error) = sandrun::run(config) {
        eprintln!("sandrun: {error}");
        std::process::exit(125);
    }
}
